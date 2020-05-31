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
	FileName    string
	FileID      string
	FolderID    string
	LastUpdated time.Time
	Channel     *drive.Channel
	lock        *sync.Mutex
}

func main() {
	logrus.SetFormatter(&logrus.JSONFormatter{PrettyPrint: true})
	logrus.SetLevel(logrus.DebugLevel)
	b, err := ioutil.ReadFile("credentials.json")
	if err != nil {
		logrus.WithError(err).Fatal("Unable to read client secret file")
	}

	// If modifying these scopes, delete your previously saved token.json.
	config, err := google.ConfigFromJSON(b, drive.DriveReadonlyScope)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to parse client secret file to conf")
	}
	driveClient = getClient(config)

	driveService, err = drive.New(driveClient)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to retrieve Drive client")
	}

	posts := subscribeToPosts()
	startHTTPListener(posts)
}

func subscribeToPosts() map[string]*Post {
	r, err := driveService.Files.List().
		Q("mimeType = 'application/vnd.google-apps.folder' and trashed = false").
		PageSize(10).Fields("nextPageToken, files(id, name)").Do()
	if err != nil {
		logrus.WithError(err).Fatal("Unable to retrieve file list")
	}

	if len(r.Files) == 0 {
		logrus.Fatal("No files found.")
	} else {
		for _, file := range r.Files {
			if file.Name == "attic-posts" {
				posts := make(map[string]*Post)
				authorFolders, err := driveService.Files.List().
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
					dateFolders, err := driveService.Files.List().
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
						postFiles, err := driveService.Files.List().
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

						returnedChannel, err := driveService.Files.Watch(postFile.Id, channel).Do()
						if err != nil {
							logrus.WithError(err).Error("Failed to subscribe to post file changes")
						}

						post := &Post{
							Author:      author.Name,
							Date:        date.Name,
							FileName:    postFile.Name,
							FileID:      postFile.Id,
							FolderID:    file.Id,
							LastUpdated: time.Now().Add(time.Duration(-2) * time.Minute),
							Channel:     returnedChannel,
							lock:        new(sync.Mutex),
						}

						logrus.WithFields(logrus.Fields{
							"channel id": returnedChannel.Id,
							"post":       post,
						}).Info("Successfully subscribed to post")

						posts[returnedChannel.Id] = post

						if err := downloadDriveFile(*post); err != nil {
							logrus.WithField("post", post).Error("Failed to download drive file after subscribing")
						}
					}
				}

				return posts
			}
		}
	}

	return nil
}

func startHTTPListener(posts map[string]*Post) {
	router := mux.NewRouter()
	logrus.Info("Starting http listener...")

	router.HandleFunc("/api", HandlePostUpdate(posts))
	router.HandleFunc("/api/stop", HandleStop(posts))

	if err := http.ListenAndServe(":9000", router); err != nil {
		logrus.WithError(err).Fatal("error starting http listener")
	}
}

func HandlePostUpdate(posts map[string]*Post) func(w http.ResponseWriter, r *http.Request) {
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
			logrus.WithField("id", id).Error("Channel ID not found for post update")
			return
		}

		post.lock.Lock()
		defer post.lock.Unlock()

		if post.LastUpdated.After(time.Now().Add(-time.Duration(1) * time.Minute)) { // post was updated in the last minute
			logrus.WithField("post", post).Debug("Post has been updated in the last minute, skipping")
			return
		}

		post.LastUpdated = time.Now()

		logrus.WithFields(logrus.Fields{
			"state":   state,
			"changes": changes,
			"post":    post,
		}).Debug("Received update notification for post")

		if err := downloadDriveFile(*post); err != nil {
			logrus.WithField("post", post).Error("Failed to download drive file after update")
		}

		return
	}
}

func downloadDriveFile(post Post) error {
	log := logrus.WithField("post", post)
	log.Info("Downloading post from Google Drive")

	// file, err := srv.Files.Get(post.Channel.ResourceId).Fields
	// if err != nil {
	// 	log.WithError(err).Error("Error downloading file from Google Drive")
	// 	return err
	// }

	// log.WithField("file", file).Debug("DEBUGGGGGGGGGGGGGGGGGGGGGGGGG")

	// fileURL := "https://docs.google.com/uc"
	fileURL := "https://googledrive.com/host/" + post.FolderID + "/" + post.Author + "/" + post.Date + "/" + post.FileName
	req, err := http.NewRequest("GET", fileURL, nil)
	if err != nil {
		log.WithError(err).Error("Failed to create GET request for updated post file")
		return err
	}

	// query := req.URL.Query()
	// query.Add("export", "download")
	// query.Add("id", post.FileID)
	// req.URL.RawQuery = query.Encode()

	log.WithField("req URL", req.URL).Debug("Attempting to GET file from google drive")
	// resp, err := driveClient.Get(fileURL)
	// log.WithField("status code", resp.StatusCode).Debug("DEBUGGGGGGGGGGGGGGGGGGGGGGGGG")
	resp, err := driveClient.Transport.RoundTrip(req)
	if err != nil {
		log.WithError(err).Error("Failed to fetch updated post file")
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.WithError(err).Error("Failed to read response body")
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("Got non-2XX status code from google drive")

		// var getError driveFileGetError
		// err2 := json.Unmarshal(body, &getError)
		// if err2 != nil {
		// 	log.WithError(err2).Error("Error unmarshalling json body into error")
		// 	return err
		// }

		log.WithFields(logrus.Fields{
			"status code": resp.StatusCode,
			"error":       resp.Body,
		}).Error(err)
		return err
	}

	log.Info("Saving updated file locally")

	postDirectory := fmt.Sprintf("./posts/%s/%s", post.Author, post.Date)
	exists, err := pathExists(postDirectory)
	if err != nil {
		log.WithError(err).Error("Error checking whether post directory exists")
		return err
	}
	if !exists {
		if err := os.MkdirAll(postDirectory, os.ModePerm); err != nil {
			log.WithError(err).Error("Error creating post directory")
			return err
		}
	}

	ioutil.WriteFile(fmt.Sprintf("./posts/%s/%s/%s", post.Author, post.Date, post.FileName), body, 0664)

	return nil
}

func HandleStop(posts map[string]*Post) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		logrus.Info("Received request to stop all listener channels")

		status := http.StatusOK
		for _, post := range posts {
			if err := driveService.Channels.Stop(post.Channel).Do(); err != nil {
				logrus.WithError(err).Error("Error stopping channel")
				status = http.StatusInternalServerError
			}
		}

		w.WriteHeader(status)
		logrus.Info("Exiting...")
		os.Exit(0)
	}
}
