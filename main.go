package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
)

// Retrieve a token, saves the token, then returns the generated client.
func getClient(config *oauth2.Config) *http.Client {
	// The file token.json stores the user's access and refresh tokens, and is
	// created automatically when the authorization flow completes for the first
	// time.
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

// Request a token from the web, then returns the retrieved token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	logrus.Infof("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		logrus.Fatalf("Unable to read authorization code %v", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		logrus.Fatalf("Unable to retrieve token from web %v", err)
	}
	return tok
}

// Retrieves a token from a local file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// Saves a token to a file path.
func saveToken(path string, token *oauth2.Token) {
	logrus.Infof("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		logrus.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func main() {
	logrus.SetFormatter(&logrus.JSONFormatter{PrettyPrint: true})
	b, err := ioutil.ReadFile("credentials.json")
	if err != nil {
		logrus.Fatalf("Unable to read client secret file: %v", err)
	}

	// If modifying these scopes, delete your previously saved token.json.
	config, err := google.ConfigFromJSON(b, drive.DriveReadonlyScope)
	if err != nil {
		logrus.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	client := getClient(config)

	srv, err := drive.New(client)
	if err != nil {
		logrus.Fatalf("Unable to retrieve Drive client: %v", err)
	}

	r, err := srv.Files.List().PageSize(10).Fields("nextPageToken, files(id, name)").Do()
	if err != nil {
		logrus.Fatalf("Unable to retrieve files: %v", err)
	}
	if len(r.Files) == 0 {
		logrus.Fatalf("No files found.")
	} else {
		for _, file := range r.Files {
			if file.Name == "attic-posts" {
				channel := &drive.Channel{
					Kind:       "api#channel",
					Id:         generateHash(10),
					ResourceId: file.Id,
					Type:       "web_hook",
					Address:    "https://theattic.us/api",
					Payload:    true,
				}
				channel, err := srv.Files.Watch(file.Id, channel).Do()
				if err != nil {
					logrus.Infof("error: %v\n", err)
				}
				logrus.Infof("channel.ServerResponse: %v\n", channel.ServerResponse)
				logrus.Infof("channel.ServerResponse.Header: %v\n", channel.ServerResponse.Header)
				startHTTPListener()
			}
		}
	}
}

func startHTTPListener() {
	router := mux.NewRouter()
	fmt.Println("starting http listener...")
	router.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		logrus.Infof("req: %v\n", r)
		w.WriteHeader(http.StatusOK)
	})
	if err := http.ListenAndServe(":9000", router); err != nil {
		logrus.Fatalf("error starting http listener: %v\n", err)
	}
}

func generateHash(length int) string {
	pool := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	rand.Seed(time.Now().UTC().UnixNano())
	b := make([]rune, length)
	for i := range b {
		b[i] = pool[rand.Intn(len(pool))]
	}
	return string(b)
}
