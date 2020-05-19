package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
)

type Post struct {
	Author   string
	Date     string
	Filename string
	FileID   string
}

func main() {
	logrus.SetFormatter(&logrus.JSONFormatter{PrettyPrint: true})
	logrus.SetLevel(logrus.InfoLevel)
	b, err := ioutil.ReadFile("credentials.json")
	if err != nil {
		logrus.WithError(err).Fatal("Unable to read client secret file")
	}

	// If modifying these scopes, delete your previously saved token.json.
	config, err := google.ConfigFromJSON(b, drive.DriveReadonlyScope)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to parse client secret file to conf")
	}
	client := getClient(config)

	srv, err := drive.New(client)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to retrieve Drive client")
	}

	r, err := srv.Files.List().PageSize(10).Fields("nextPageToken, files(id, name)").Do()
	if err != nil {
		logrus.WithError(err).Fatal("Unable to retrieve file list")
	}
	if len(r.Files) == 0 {
		logrus.Fatal("No files found.")
	} else {
		for _, file := range r.Files {
			if file.Name == "attic-posts" {
				posts := make(map[string]Post)
				authorFolders, err := srv.Files.List().
					Q(fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", file.Id)).
					PageSize(100).Fields("nextPageToken, files(id, name)").Do()
				if err != nil {
					logrus.WithError(err).Fatal("Error listing author folders")
				}
				for _, author := range authorFolders.Files {
					logrus.WithField("author", author.Name).Debug("Retrieving posts for author")
					dateFolders, err := srv.Files.List().
						Q(fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", author.Id)).
						PageSize(100).Fields("nextPageToken, files(id, name)").Do()
					if err != nil {
						logrus.WithError(err).WithField("author", author).Fatal("Error listing author post folders")
					}

					for _, date := range dateFolders.Files {
						logrus.WithField("date", date.Name).Debug("Retrieving posts for author")
						postFiles, err := srv.Files.List().
							Q(fmt.Sprintf("(mimeType = 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' or mimeType = 'application/vnd.google-apps.document') and '%s' in parents and trashed = false", date.Id)).
							PageSize(1).Fields("nextPageToken, files(id, name)").Do()
						if err != nil {
							logrus.WithError(err).Fatal("Error retrieving post file")
						}

						if len(postFiles.Files) != 1 {
							logrus.WithFields(logrus.Fields{
								"actual":   len(postFiles.Files),
								"expected": 1,
							}).Error("Unexpected number of post files")
						}

						postFile := postFiles.Files[0]

						channelID := generateHash(10)
						channel := &drive.Channel{
							Kind:       "api#channel",
							Id:         channelID,
							ResourceId: postFile.Id,
							Type:       "web_hook",
							Address:    "https://theattic.us/api",
							Payload:    true,
						}

						returnedChannel, err := srv.Files.Watch(postFile.Id, channel).Do()
						if err != nil {
							logrus.WithError(err).Error("error subscribing to post file changes")
						}

						post := Post{
							Author:   author.Name,
							Date:     date.Name,
							Filename: postFile.Name,
							FileID:   postFile.Id,
						}
						logrus.WithFields(logrus.Fields{
							"channel id": returnedChannel.Id,
							"post":       post,
						}).Info("Have post")
						posts[returnedChannel.Id] = post
					}
				}
				startHTTPListener(posts)
			}
		}
	}
}

func startHTTPListener(posts map[string]Post) {
	router := mux.NewRouter()
	fmt.Println("starting http listener...")
	router.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			logrus.WithError(err).Error("Error reading request body")
			return
		}

		logrus.WithFields(logrus.Fields{
			"header": r.Header,
			"body":   body,
		}).Debug("Have request")

		state := r.Header.Get("X-Goog-Resource-State")
		if state != "update" {
			return
		}

		changes := strings.Split(r.Header.Get("X-Goog-Changed"), ", ")
		contentChange := false
		for _, change := range changes {
			if change == "content" {
				contentChange = true
			}
		}
		if !contentChange {
			return
		}

		logrus.Info("Handling update request")

		id := r.Header.Get("X-Goog-Channel-ID")
		post, ok := posts[id]
		if !ok {
			logrus.WithField("id", id).Error("resource ID not found for post update")
			return
		}

		logrus.WithField("post", post).Info("Received update notification for post")

		return
	})
	if err := http.ListenAndServe(":9000", router); err != nil {
		logrus.WithError(err).Fatal("error starting http listener")
	}
}

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
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		logrus.WithError(err).Fatal("Unable to read authorization code")
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to retrieve token from file")
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
	logrus.WithField("path", path).Info("Saving credential file")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to cache oauth tok")
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
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
