package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	gdrive "google.golang.org/api/drive/v2"

	"net/http"

	"golang.org/x/oauth2"
)

// Drive holds the Google Drive API connection(s)
type Drive struct {
	Cache           Cache
	context         context.Context
	activeAccountID int
	accounts        []Account
	tokens          []oauth2.Token
	configs         []oauth2.Config
	maxDelay        int
	chunkDir        string
}

// NewDriveClient creates a new Google Drive instance
func NewDriveClient(accounts []Account, tokenPath string, chunkDir string) (*Drive, error) {
	drive := Drive{
		activeAccountID: 1,
		context:         context.Background(),
		accounts:        accounts,
		maxDelay:        5000,
		chunkDir:        chunkDir,
	}

	if err := drive.authorize(tokenPath); nil != err {
		return nil, err
	}

	go drive.startAutoRefresh()

	return &drive, nil
}

func (d *Drive) startAutoRefresh() {
	client, err := d.getClient()
	if nil != err {
		log.Printf("Could not get client for auto refreshing")
		return
	}
	lastCheck := time.Now()

	for _ = range time.Tick(10 * time.Minute) {
		log.Printf("Checking for updates...")
		checkDate := lastCheck.Format(time.RFC3339)
		lastCheck = time.Now()
		pageToken := ""
		for {
			query := client.Files.List().Q(fmt.Sprintf("modifiedTime > '%v'", checkDate))

			if "" != pageToken {
				query = query.PageToken(pageToken)
			}

			r, err := query.Do()
			if nil != err {
				break
			}

			for _, file := range r.Items {
				object := mapDriveToAPIObject(file)
				log.Printf("Updated file %v (%v)", object.ID, object.Name)
				if err := d.Cache.Store(object); nil != err {
					log.Printf("Could not refresh %v", object.ID)
				}
			}
			pageToken = r.NextPageToken

			if "" == pageToken {
				break
			}
		}
	}
}

// FileSize gets the file size
func (d *Drive) FileSize(id string) (int64, error) {
	client, err := d.getClient()
	if nil != err {
		return 0, err
	}

	httpResponse, err := client.Files.Get(id).Download()
	if nil != err {
		return 0, err
	}

	statusCode := httpResponse.StatusCode
	if 200 == statusCode {
		return httpResponse.ContentLength, nil
	}

	return 0, fmt.Errorf("Invalid status code %v", statusCode)
}

func arrayIndex(values []string, value string) int {
	for p, v := range values {
		if v == value {
			return p
		}
	}
	return -1
}

// Open a file
func (d *Drive) Open(object *APIObject, chunkSize int64) (*Buffer, error) {
	nativeClient := d.getNativeClient()
	return GetBufferInstance(nativeClient, object, chunkSize, d.chunkDir)
}

// GetObject gets one object by id
func (d *Drive) GetObject(id string) (*APIObject, error) {
	client, err := d.getClient()
	if nil != err {
		return nil, err
	}

	o, err := client.Files.Get(id).Do()
	if nil != err {
		return nil, err
	}

	if o.FileSize == 0 {
		fileSize, err := d.FileSize(id)
		if nil != err {
			fileSize = o.FileSize
		}
		o.FileSize = fileSize
	}

	return mapDriveToAPIObject(o), nil
}

// GetObjectsByParent gets all files under a parent folder
func (d *Drive) GetObjectsByParent(parentID string) ([]*APIObject, error) {
	client, err := d.getClient()
	if nil != err {
		return nil, err
	}

	var files []*APIObject
	pageToken := ""
	for {
		query := client.Files.List().Q(fmt.Sprintf("'%v' in parents AND trashed = false", parentID))

		if "" != pageToken {
			query = query.PageToken(pageToken)
		}

		r, err := query.Do()
		if nil != err {
			break
		}

		for _, file := range r.Items {
			files = append(files, mapDriveToAPIObject(file))
		}
		pageToken = r.NextPageToken

		if "" == pageToken {
			break
		}
	}

	return files, nil
}

// GetFileByNameAndParent gets a file
func (d *Drive) GetFileByNameAndParent(name, parent string) (*gdrive.File, error) {
	client, err := d.getClient()
	if nil != err {
		return nil, err
	}

	r, err := client.Files.List().Q(fmt.Sprintf("'%v' in parents AND name = '%v' AND trashed = false", parent, name)).Do()
	if nil != err {
		return nil, err
	}

	for _, f := range r.Items {
		if name == f.Title {
			return f, nil
		}
	}
	return nil, fmt.Errorf("Could not find %s in directory %v", name, parent)
}

func (d *Drive) authorize(tokenPath string) error {
	d.tokens = getTokens(tokenPath)
	if len(d.tokens) < len(d.accounts) {
		for _, account := range d.accounts {
			config := oauth2.Config{
				ClientID:     account.ClientID,
				ClientSecret: account.ClientSecret,
				Endpoint: oauth2.Endpoint{
					AuthURL:  "https://accounts.google.com/o/oauth2/auth",
					TokenURL: "https://accounts.google.com/o/oauth2/token",
				},
				RedirectURL: "urn:ietf:wg:oauth:2.0:oob",
				Scopes:      []string{gdrive.DriveScope},
			}
			token := getTokenFromWeb(&config)
			d.configs = append(d.configs, config)
			d.tokens = append(d.tokens, *token)
		}
		if err := storeTokens(tokenPath, d.tokens); nil != err {
			return err
		}
	} else {
		for _, account := range d.accounts {
			config := oauth2.Config{
				ClientID:     account.ClientID,
				ClientSecret: account.ClientSecret,
				Endpoint: oauth2.Endpoint{
					AuthURL:  "https://accounts.google.com/o/oauth2/auth",
					TokenURL: "https://accounts.google.com/o/oauth2/token",
				},
				RedirectURL: "urn:ietf:wg:oauth:2.0:oob",
				Scopes:      []string{gdrive.DriveScope},
			}
			d.configs = append(d.configs, config)
		}
	}

	return nil
}

func (d *Drive) getClient() (*gdrive.Service, error) {
	client := d.configs[d.activeAccountID-1].Client(d.context, &d.tokens[d.activeAccountID-1])
	return gdrive.New(client)
}

func (d *Drive) getNativeClient() *http.Client {
	return oauth2.NewClient(d.context, d.configs[d.activeAccountID-1].TokenSource(d.context, &d.tokens[d.activeAccountID-1]))
}

func (d *Drive) rotateAccounts() {
	if (d.activeAccountID + 1) > len(d.configs) {
		d.activeAccountID = 1
	} else {
		d.activeAccountID++
	}
	log.Printf("Usage limit exceeded, rotating accounts to account #%v", d.activeAccountID)
}

func getTokens(tokenPath string) []oauth2.Token {
	var tokens []oauth2.Token
	tokenFile, err := ioutil.ReadFile(tokenPath)
	if nil != err {
		return tokens
	}
	json.Unmarshal(tokenFile, &tokens)
	return tokens
}

func storeTokens(tokenPath string, tokens []oauth2.Token) error {
	j, err := json.Marshal(tokens)
	if nil != err {
		return fmt.Errorf("Could not store tokens, %v", err)
	}
	ioutil.WriteFile(tokenPath, j, 0644)
	return nil
}

// getTokenFromWeb uses Config to request a Token.
// It returns the retrieved Token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser %v\n", authURL)
	fmt.Printf("Paste the authorization code: ")

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		log.Fatalf("Unable to read authorization code %v", err)
	}

	tok, err := config.Exchange(oauth2.NoContext, code)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web %v", err)
	}
	return tok
}

func mapDriveToAPIObject(file *gdrive.File) *APIObject {
	mtime, err := time.Parse(time.RFC3339, file.ModifiedDate)
	if nil != err {
		mtime = time.Now()
	}

	var parents []string
	for _, parent := range file.Parents {
		parents = append(parents, parent.Id)
	}

	return &APIObject{
		ID:          file.Id,
		Parents:     parents,
		Name:        file.Title,
		IsDir:       file.MimeType == "application/vnd.google-apps.folder",
		Size:        uint64(file.FileSize),
		MTime:       mtime,
		DownloadURL: file.DownloadUrl,
	}
}
