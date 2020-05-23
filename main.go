package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
)

type Post struct {
	Author      string
	Date        string
	Filename    string
	LastUpdated time.Time
	Channel     *drive.Channel
	lock        *sync.Mutex
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

	srv, err = drive.New(client)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to retrieve Drive client")
	}

	posts := subscribeToPosts()
	startHTTPListener(posts)
}

func subscribeToPosts() map[string]Post {
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
					PageSize(1).Fields("nextPageToken, files(id, name)").Do()
				if err != nil {
					logrus.WithError(err).Fatal("Error listing author folders")
				}

				/*************************
				* get all author folders *
				*************************/

				for _, author := range authorFolders.Files {
					logrus.WithField("author", author.Name).Debug("Retrieving posts for author")
					dateFolders, err := srv.Files.List().
						Q(fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", author.Id)).
						PageSize(100).Fields("nextPageToken, files(id, name)").Do()
					if err != nil {
						logrus.WithError(err).WithField("author", author).Fatal("Error listing author post folders")
					}

					/**********************************
					* get all post folders for author *
					**********************************/

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

						/************************************
						* subscribe to updates on post file *
						************************************/

						postFile := postFiles.Files[0]

						channelID := generateHash(10)
						expiration := time.Now().Add(time.Duration(1)*time.Minute).UnixNano() / 1000000
						channel := &drive.Channel{
							Kind:       "api#channel",
							Id:         channelID,
							Expiration: expiration,
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
							Author:      author.Name,
							Date:        date.Name,
							Filename:    postFile.Name,
							LastUpdated: time.Now(),
							Channel:     returnedChannel,
						}

						logrus.WithFields(logrus.Fields{
							"channel id": returnedChannel.Id,
							"post":       post,
						}).Info("Have post")

						posts[returnedChannel.Id] = post
					}
				}

				return posts
			}
		}
	}

	return nil
}

func startHTTPListener(posts map[string]Post) {
	router := mux.NewRouter()
	logrus.Info("Starting http listener...")

	router.HandleFunc("/api", HandlePostUpdate(posts))
	router.HandleFunc("/api/stop", HandleStop(posts))

	if err := http.ListenAndServe(":9000", router); err != nil {
		logrus.WithError(err).Fatal("error starting http listener")
	}
}

func HandlePostUpdate(posts map[string]Post) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			logrus.WithError(err).Error("Error reading request body")
			return
		}

		state := r.Header.Get("X-Goog-Resource-State")
		if state != "update" {
			return
		}

		logrus.WithFields(logrus.Fields{
			"header": r.Header,
			"body":   body,
		}).Debug("Have request")

		var changes []string
		for _, change := range strings.Split(r.Header.Get("X-Goog-Changed"), ",") {
			if change == "content" || change == "properties" {
				changes = append(changes, change)
			}
		}
		if len(changes) == 0 {
			return
		}

		id := r.Header.Get("X-Goog-Channel-ID")
		post, ok := posts[id]
		if !ok {
			logrus.WithField("id", id).Error("resource ID not found for post update")
			return
		}

		post.lock.Lock()
		defer post.lock.Unlock()

		if post.LastUpdated.After(time.Now().Add(time.Duration(-1) * time.Minute)) {
			logrus.WithField("post", post).Debug("Post has been updated in the last minute, skipping")
			return
		}

		post.LastUpdated = time.Now()

		logrus.WithFields(logrus.Fields{
			"state":   state,
			"changes": changes,
			"post":    post,
		}).Debug("Received update notification for post")

		downloadDriveFile(post)

		return
	}
}

func downloadDriveFile(post Post) {
	log := logrus.WithField("post", post)
	log.Info("Handling update request")

	req, err := http.NewRequest("GET", post.Channel.ResourceUri, nil)
	if err != nil {
		log.WithError(err).Error("Failed to create GET request for updated post file")
		return
	}

	resp, err := http.DefaultTransport.RoundTrip(req)
	defer resp.Body.Close()
	if err != nil {
		log.WithError(err).Error("Failed to fetch updated post file")
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.WithError(err).Error("Failed to read response body")
		return
	}

	log.WithField("body", body).Info("Have post file body")

	return
}

func HandleStop(posts map[string]Post) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		logrus.Info("Received request to stop all listener channels")

		status := http.StatusOK
		for _, post := range posts {
			if err := srv.Channels.Stop(post.Channel).Do(); err != nil {
				logrus.WithError(err).Error("Error stopping channel")
				status = http.StatusInternalServerError
			}
		}

		w.WriteHeader(status)
		logrus.Info("Exiting...")
		os.Exit(0)
	}
}
