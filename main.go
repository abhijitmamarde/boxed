package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"

	"github.com/boltdb/bolt"
	"github.com/gorilla/sessions"
	"github.com/julienschmidt/httprouter"
	"github.com/markbates/going/wait"
	"github.com/tejo/boxed/datastore"
	"github.com/tejo/boxed/dropbox"
)

func main() {
	datastore.Connect("blog.db")
	defer datastore.Close()

	handleCommands()

	router := httprouter.New()
	router.GET("/", Index)
	router.GET("/login", Login)
	router.GET(config.WebHookURL, WebHook)
	router.POST(config.WebHookURL, WebHook)
	router.GET("/account", Account)
	router.GET(config.CallbackURL, Callback)

	log.Fatal(http.ListenAndServe(config.Port, router))
}

func Login(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	withSession(w, r, func(session *sessions.Session) {
		RequestToken, _ := dropbox.StartAuth(config.AppToken)
		session.Values["RequestToken"] = RequestToken
		url, _ := url.Parse(config.HostWithProtocol + config.CallbackURL)
		authURL := dropbox.GetAuthorizeURL(RequestToken, url)
		session.Save(r, w)
		http.Redirect(w, r, authURL.String(), 302)
	})
}

// saves the user id in session, save used data and access token in
// db, creates the default folders
func Callback(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	withSession(w, r, func(session *sessions.Session) {
		RequestToken := session.Values["RequestToken"].(dropbox.RequestToken)
		AccessToken, _ := dropbox.FinishAuth(config.AppToken, RequestToken)
		dbc := dropbox.NewClient(AccessToken, config.AppToken)
		info, err := dbc.GetAccountInfo()
		if err != nil {
			log.Println(err)
		}
		datastore.SaveUserData(info, AccessToken)
		session.Values["email"] = info.Email
		session.Save(r, w)
		dbc.CreateDir("drafts")
		dbc.CreateDir("published")
		http.Redirect(w, r, "/", 302)
	})
}

func WebHook(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	if r.Method == "GET" {
		fmt.Fprintf(w, "%s", r.URL.Query().Get("challenge"))
		return
	}

	if r.Method == "POST" {
		decoder := json.NewDecoder(r.Body)
		var d dropbox.DeltaPayLoad
		err := decoder.Decode(&d)
		if err != nil {
			log.Println(err)
			return
		}
		log.Printf("processing %+v\n", d.Delta.Users)
		go processChanges(d.Delta.Users)
	}
}

func refreshArticles(email string) {
	datastore.DeleteArtilcles(email)
	at, err := datastore.LoadUserToken(email)
	if err != nil {
		log.Fatal(err)
		return
	}
	dbc := dropbox.NewClient(at, config.AppToken)
	meta, _ := dbc.GetMetadata("/published", true)
	wait.Wait(len(meta.Contents), func(index int) {
		entry := meta.Contents[index]
		if entry.IsDir {
			return
		}
		file, _ := dbc.GetFile(entry.Path)
		content, _ := ioutil.ReadAll(file)
		article := datastore.ParseEntry(entry, content)
		article.GenerateID(email)
		article.Save()
		log.Printf("processed rev: %s  path:%s\n", article.Rev, article.Path)
	})
}

func processChanges(users []int) {
	for _, v := range users {
		email, err := datastore.GetUserEmailByUID(v)
		if err == nil {
			currentCursor, _ := datastore.GetCurrenCursorByEmail(email)
			go processUserDelta(email, currentCursor)
		}
	}

}

func processUserDelta(email, cursor string) {
	at, err := datastore.LoadUserToken(email)
	if err != nil {
		log.Fatal(err)
		return
	}
	dbc := dropbox.NewClient(at, config.AppToken)
	d, _ := dbc.GetDelta("/published", cursor)
	datastore.SaveCurrentCursor(email, d.Cursor)
	fmt.Printf("d = %+v\n", d)
	wait.Wait(len(d.Updated), func(index int) {
		entry, _ := dbc.GetMetadata(d.Updated[index], true)
		file, _ := dbc.GetFile(d.Updated[index])
		content, _ := ioutil.ReadAll(file)
		article := datastore.ParseEntry(*entry, content)
		article.GenerateID(email)
		article.Save()
		log.Printf("updated: %s", article.Path)
	})
	for _, v := range d.Deleted {
		a, err := datastore.LoadArticleByComputedPath(email + ":" + v)
		if err == nil {
			a.Delete()
		}
	}
}

// func ArticleHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
// 	fmt.Printf("ps = %+v\n", ps.ByName("articleslug"))
// 	db.View(func(tx *bolt.Tx) error {
// 		c := tx.Bucket([]byte("UserArticles")).Cursor()

// 		prefix := []byte(defaultUserEmail + ":article:")
// 		for k, v := c.Seek(prefix); bytes.HasPrefix(k, prefix); k, v = c.Next() {
// 			var a Article
// 			json.Unmarshal(v, &a)
// 			fmt.Fprint(w, a.Path)
// 		}

// 		return nil
// 	})
// }

func Index(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	datastore.DB.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("UserArticles")).Cursor()

		prefix := []byte(config.DefaultUserEmail + ":article:")
		for k, v := c.Seek(prefix); bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var a datastore.Article
			json.Unmarshal(v, &a)
			fmt.Fprint(w, a)
		}

		return nil
	})
}

func Account(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	withSession(w, r, func(session *sessions.Session) {
		var AccessToken dropbox.AccessToken

		if email := session.Values["email"]; email == nil {
			fmt.Fprint(w, "no email found")
			return
		}
		email := session.Values["email"].(string)
		AccessToken, _ = datastore.LoadUserToken(email)

		dbc := dropbox.NewClient(AccessToken, config.AppToken)
		info, err := dbc.GetAccountInfo()
		if err != nil {
			// access token is not valid anymore
			// reset session
			session.Values["email"] = ""
			session.Save(r, w)
			fmt.Fprint(w, "access token not valid")
			return
		}
		fmt.Fprintf(w, "info = %+v\n", info)

		// dropbox.Debug = true
		currentCursor, _ := datastore.GetCurrenCursorByEmail(email)
		processUserDelta(email, currentCursor)
	})
}

func withSession(w http.ResponseWriter, r *http.Request, fn func(*sessions.Session)) {
	gob.Register(dropbox.RequestToken{})
	store := sessions.NewCookieStore([]byte("182hetsgeih8765$aasdhj"))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 30 * 12,
		HttpOnly: true,
	}
	session, _ := store.Get(r, "godropblog")
	fn(session)
}
