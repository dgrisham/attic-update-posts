package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	MimeType    string
	LastUpdated time.Time
	Channel     *drive.Channel
	image       *drive.File
	lock        *sync.Mutex
}

const DEBUG = true

func main() {
	logrus.SetFormatter(&logrus.JSONFormatter{PrettyPrint: true})
	if DEBUG {
		logrus.SetLevel(logrus.DebugLevel)
	} else {
		logrus.SetLevel(logrus.InfoLevel)
	}

	logrus.Info("Starting up update-posts")
	logrus.Info("Successfully set up logger")

	b, err := ioutil.ReadFile("/home/grish/update-posts/credentials.json")
	if err != nil {
		logrus.WithError(err).Fatal("Unable to read client secret file")
	}

	// If modifying these scopes, delete your previously saved token.json.
	config, err := google.ConfigFromJSON(b, drive.DriveReadonlyScope)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to parse client secret file to conf")
	}
	driveClient = getClient(config)
	driveClient.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		r.URL.Opaque = r.URL.Path
		return nil
	}

	logrus.Info("Initializing drive service...")
	driveService, err = drive.New(driveClient)
	if err != nil {
		logrus.WithError(err).Fatal("Unable to retrieve Drive client")
	}
	logrus.Info("Successfully initialized drive service")

	posts, err := subscribeToPosts()
	if err != nil {
		logrus.WithError(err).Fatal("Failed to subscribe to posts, exiting")
	}
	startHTTPListener(posts)
}

func subscribeToPosts() (map[string]*Post, error) {
	logrus.Debug("Getting lists of files to subscribe to")
	r, err := driveService.Files.List().
		Q("mimeType = 'application/vnd.google-apps.folder' and name = 'attic-posts'").
		PageSize(1).Fields("nextPageToken, files(id, name)").Do()
	if err != nil {
		return nil, fmt.Errorf("Error querying google drive for posts folder: %s", err.Error())
	}

	if len(r.Files) == 0 {
		return nil, fmt.Errorf("attic-posts folder not found")
	}

	logrus.Debug("Found attic-posts folder")
	folder := r.Files[0]

	posts := make(map[string]*Post)
	authorFolders, err := driveService.Files.List().
		Q(fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", folder.Id)).
		PageSize(15).Fields("nextPageToken, files(id, name)").Do()
	if err != nil {
		return nil, fmt.Errorf("Error getting list of author folders: %s", err.Error())
	}

	/*************************
	* get all author folders *
	*************************/

	logrus.Debug("Getting all author folders")
	for i, author := range authorFolders.Files {

		if DEBUG && i > 0 {
			logrus.Debug("First author processed, skipping rest")
			return posts, nil
		}

		logrus.WithField("author", author.Name).Debug("Retrieving posts for author")
		dateFolders, err := driveService.Files.List().
			Q(fmt.Sprintf("mimeType = 'application/vnd.google-apps.folder' and '%s' in parents and trashed = false", author.Id)).
			PageSize(10).Fields("nextPageToken, files(id, name)").Do()
		if err != nil {
			return nil, fmt.Errorf("Error listing post folders for author '%s': %s", author.Name, err.Error())
		}

		/**********************************
		* get all post folders for author *
		**********************************/

		for _, date := range dateFolders.Files {

			logrus.WithField("date", date.Name).Debug("Retrieving post for author")
			postFiles, err := driveService.Files.List().
				Q(fmt.Sprintf("(mimeType = '%s' or mimeType = '%s') and '%s' in parents and trashed = false", docxMime, googleDocMime, date.Id)).
				PageSize(1).Fields("files(id, name, mimeType)").Do()
			if err != nil {
				return nil, fmt.Errorf("Error retrieving post file: %s", err.Error())
			}

			if len(postFiles.Files) != 1 {
				logrus.WithFields(logrus.Fields{
					"actual":   len(postFiles.Files),
					"expected": 1,
				}).Error("Unexpected number of post files")
				continue
			}

			logrus.WithField("date", date.Name).Debug("Retrieving image for post")
			imageFiles, err := driveService.Files.List().
				Q(fmt.Sprintf("mimeType = '%s' and '%s' in parents and trashed = false", jpegMime, date.Id)).
				PageSize(1).Fields("files(id, name, mimeType)").Do()
			if err != nil {
				return nil, fmt.Errorf("Error retrieving image file: %s", err.Error())
			}

			if len(imageFiles.Files) != 1 {
				logrus.WithFields(logrus.Fields{
					"actual":   len(imageFiles.Files),
					"expected": 1,
				}).Error("Unexpected number of image files")
				continue
			}

			/************************************
			* subscribe to updates on post file *
			************************************/

			postFile := postFiles.Files[0]
			imageFile := imageFiles.Files[0]

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
				MimeType:    postFile.MimeType,
				LastUpdated: time.Now().Add(time.Duration(-2) * time.Minute),
				Channel:     returnedChannel,
				image:       imageFile,
				lock:        new(sync.Mutex),
			}

			logrus.WithFields(logrus.Fields{
				"channel id": returnedChannel.Id,
				"post":       post,
			}).Info("Successfully subscribed to post")

			posts[returnedChannel.Id] = post

			if err := downloadPost(*post); err != nil {
				logrus.WithError(err).WithField("post", post).Error("Failed to download drive file after subscribing")
			}
		}
	}

	return posts, nil
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

		if err := downloadPost(*post); err != nil {
			logrus.WithField("post", post).Error("Failed to download drive file after update")
		}

		return
	}
}

func downloadPost(post Post) error {
	log := logrus.WithField("post", post)
	log.Info("Downloading post from Google Drive")

	postDirectory := fmt.Sprintf("/home/grish/html/drive/%s/%s", post.Author, post.Date)

	/******************************
	* download and save post file *
	******************************/

	postPath := fmt.Sprintf("%s/%s", postDirectory, post.FileName)
	{
		// download post file
		body, err := downloadDriveFile(post.FileID, post.MimeType)
		if err != nil {
			log.WithError(err).Error("Error downloading post")
			return err
		}

		log.WithField("postPath", postPath).Info("Saving post file locally")

		// ensure post directory exists
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

		// save post file
		if err := ioutil.WriteFile(postPath, body, 0664); err != nil {
			log.WithError(err).Error("Error saving post to local file")
			return err
		}
	}

	/****************************
	* ensure cover image exists *
	*****************************/

	imagePath := fmt.Sprintf("%s/%s", postDirectory, post.image.Name)
	// imageDownloaded := false
	{
		exists, err := pathExists(imagePath)
		if err != nil {
			log.WithError(err).Error("Error checking whether post directory exists")
			return err
		}
		if !exists {
			log.WithField("imagePath", imagePath).Info("Downloading post image")
			body, err := downloadDriveFile(post.image.Id, post.image.MimeType)
			if err != nil {
				log.WithError(err).Error("Error downloading post")
				return err
			}

			// save image file
			if err := ioutil.WriteFile(imagePath, body, 0664); err != nil {
				log.WithError(err).Error("Error saving image file")
				return err
			}

			// imageDownloaded = true
		}
	}

	/*****************************************
	* make sure output html directory exists *
	*****************************************/

	htmlDirectory := fmt.Sprintf("/home/grish/html/html/posts/%s/%s", post.Author, post.Date)
	{
		exists, err := pathExists(htmlDirectory)
		if err != nil {
			log.WithError(err).Error("Error checking whether html destination directory exists")
			return err
		}
		if !exists {
			if err := os.MkdirAll(htmlDirectory, os.ModePerm); err != nil {
				log.WithError(err).Error("Error creating html destination directory")
				return err
			}
		}
	}

	/****************************
	* convert post file to html *
	****************************/

	{
		var args []string
		args = append(args, "/home/grish/html/bin/convert_posts.zsh", "post")
		args = append(args, postPath, htmlDirectory)

		log.WithField("cmd", strings.Join(args, " ")).Info("Running script to update post html from docx")

		cmd := exec.Command(args[0], args[1:]...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			log.WithError(err).WithField("stderr", stderr.String()).Error("Failed to run script to update post html from docx")
			return err
		}

		log.WithField("stdout", stdout.String()).Info("Successfully ran script to update post html from docx")
	}

	/**********************************************************
	* if we downloaded a new cover image, generate thumbnails *
	**********************************************************/

	{
		var args []string
		title := strings.TrimSuffix(post.FileName, filepath.Ext(post.FileName))
		args = append(args, "/home/grish/html/bin/make_thumbnail.zsh", title, post.Author, imagePath, htmlDirectory)

		log.WithField("cmd", strings.Join(args, " ")).Info("Running script to create thumbnails from cover image")

		cmd := exec.Command(args[0], args[1:]...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			log.WithError(err).WithField("stderr", stderr.String()).Error("Failed to run script to update post html from docx")
			return err
		}

		log.WithField("stdout", stdout.String()).Debug("Successfully ran script to update post html from docx")
	}

	/**************************
	* regenerate the homepage *
	**************************/

	{
		script := "/home/grish/html/bin/gen_homepage.zsh"

		log.WithField("cmd", script).Info("Running script to generate homepage")

		cmd := exec.Command(script)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			log.WithError(err).WithField("stderr", stderr.String()).Error("Failed to run script to generate homepage")
			return err
		}

		log.WithField("stdout", stdout.String()).Debug("Successfully ran script to generate homepage")
	}

	/*****************************************
	* rsync html directory with website root *
	*****************************************/

	{
		var args []string
		args = append(args, "/usr/local/bin/sudo", "/usr/local/bin/rsync", "-rl", "--delete", "/home/grish/html/html", "/usr/local/www")

		log.WithField("cmd", strings.Join(args, " ")).Debug("Running command to sync html posts to attic root")

		cmd := exec.Command(args[0], args[1:]...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			log.WithError(err).WithField("stderr", stderr.String()).Error("Failed to run command to sync html posts to attic root")
			return err
		}

		log.WithField("stdout", stdout.String()).Debug("Successfully ran command to sync html posts to attic root")
	}

	return nil
}

func downloadDriveFile(fileID string, mimeType string) ([]byte, error) {
	log := logrus.WithFields(logrus.Fields{
		"fileID":   fileID,
		"mimeType": mimeType,
	})

	var resp *http.Response
	var err error
	switch mimeType {
	case docxMime, jpegMime: // download docx directly
		resp, err = driveService.Files.Get(fileID).Download()
	case googleDocMime: // export google doc files as docx
		resp, err = driveService.Files.Export(fileID, docxMime).Download()
	default:
		return nil, fmt.Errorf("unsupported mime type: %s", mimeType)
	}
	if err != nil {
		log.WithError(err).Error("Failed to fetch file from Google Drive")
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.WithError(err).Error("Failed to read response body")
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("Got non-2XX status code from Google Drive")

		var getError driveFileGetError
		err2 := json.Unmarshal(body, &getError)
		if err2 != nil {
			log.WithError(err2).Error("Error unmarshalling json body into error")
			return nil, err
		}

		log.WithFields(logrus.Fields{
			"status code": resp.StatusCode,
			"json error":  err,
		}).Error(err)
		return nil, err
	}

	return body, nil
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
