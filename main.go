package main

// go get github.com/mattn/go-sqlite3 github.com/go-telegram-bot-api/telegram-bot-api mvdan.cc/xurls github.com/sirupsen/logrus

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	_ "github.com/mattn/go-sqlite3"
	logrus "github.com/sirupsen/logrus"
	xurls "mvdan.cc/xurls"
)

var log *logrus.Logger
var signalCh chan os.Signal
var ctx context.Context
var cancel context.CancelFunc

func init() {
	log = logrus.New()
	log.Out = os.Stdout

	botInfo.readConfig()

	log.Info("Init")

	signalCh = make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, os.Kill,
		syscall.SIGINT,
		syscall.SIGTERM)

	ctx, cancel = context.WithCancel(context.Background())
}

type BotInfo struct {
	Token        string   `json:"token"`
	Site         string   `json:"wallabag_site"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Username     string   `json:"username"`
	Password     string   `json:"password"`
	FilterUsers  []string `json:"filter_users"`
}

var botInfo BotInfo

func (b *BotInfo) readConfig() {
	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}

	configFile := filepath.Join(usr.HomeDir, ".config", "t.me", "wallabag.json")

	if _, err := os.Stat(configFile); err != nil {
		log.Fatalf("Fail to open config file %s with error: %v", configFile, err)
	}

	jsonFile, err := os.Open(configFile)
	if err != nil {
		fmt.Println(err)
	}
	defer jsonFile.Close()

	var content []byte

	if content, err = ioutil.ReadAll(jsonFile); err != nil {
		log.Fatalf("Fail to read bot config: %v", err)
	}

	if err := json.Unmarshal(content, &b); err != nil {
		log.Fatalf("Fail to parse bot config: %v", err)
	}

	if b.Token == "" || b.Site == "" || b.ClientID == "" || b.ClientSecret == "" || b.Username == "" || b.Password == "" {
		log.Fatalf("Fail to parse bot token and wallabag credentials")
	}
}

type saveURLRequest struct {
	URL       string
	ChatID    int64
	MessageID int
}

const rescanInterval = 3600

func sqlite3Handler(diskQueue, reqQueue, diskAckQueue, ackQueue chan saveURLRequest) {
	var database *sql.DB
	var statement *sql.Stmt
	var err error
	database, err = sql.Open("sqlite3", "./wallabag.db")

	if err != nil {
		log.Fatalf("%v", err)
	}

	if statement, err = database.Prepare(`CREATE TABLE IF NOT EXISTS Requests (	
			id INTEGER PRIMARY KEY AUTOINCREMENT, 
			URL TEXT, 
			ChatID INTEGER,
			MessageID INTEGER,
			saved INTEGER)`); err != nil {
		log.Fatalf("%v", err)
	}

	if _, err = statement.Exec(); err != nil {
		log.Fatalf("%v", err)
	}

	if statement, err = database.Prepare(`CREATE INDEX IF NOT EXISTS URLIndex ON Requests(URL);`); err != nil {
		log.Fatalf("%v", err)
	}

	if _, err = statement.Exec(); err != nil {
		log.Fatalf("%v", err)
	}

	go func() {

		for {
			timer := time.NewTimer(rescanInterval * time.Second)

			var rows *sql.Rows
			if rows, err = database.Query(`
		SELECT URL, ChatID, MessageID
		FROM Requests
		WHERE saved == 0
		`); err != nil {
				log.Fatalf("%v", err)
			}

			var URL string
			var ChatID int64
			var MessageID int

			for rows.Next() {
				if err := rows.Scan(&URL, &ChatID, &MessageID); err != nil {
					log.Error("Cannot read url from database")
					continue
				}
				log.Infof("Unfinished %s, %d, %d", URL, ChatID, MessageID)
				reqQueue <- saveURLRequest{URL: URL, ChatID: ChatID, MessageID: MessageID}
			}

			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
	}()

	go func() {
		for {
			var r saveURLRequest
			select {
			case r = <-diskQueue:
			case <-ctx.Done():
				return
			}

			var count int
			row := database.QueryRow("SELECT COUNT(*) FROM Requests WHERE URL = ?", r.URL)
			err := row.Scan(&count)

			if err != nil {
				log.Errorf("Fail to get count of URL: %v", err)
				continue
			}

			if count != 0 {
				log.Infof("Skip existing URL: %s", r.URL)
				continue
			}

			log.Infof("Saving request to disk first: %s, %d, %d", r.URL, r.ChatID, r.MessageID)

			statement, err := database.Prepare("INSERT INTO Requests (URL, ChatID, MessageID, saved) VALUES (?, ?, ?, 0)")
			if err != nil {
				log.Errorf("Fail to insert request to SQLite: %v", err)
				continue
			}
			_, err = statement.Exec(r.URL, r.ChatID, r.MessageID)
			if err != nil {
				log.Errorf("Fail to insert request to SQLite: %v", err)
				continue
			}
			reqQueue <- r
		}
	}()

	go func() {
		for {
			var r saveURLRequest
			select {
			case r = <-diskAckQueue:
			case <-ctx.Done():
				return
			}
			log.Infof("Update URL as saved: %s, %d, %d", r.URL, r.ChatID, r.MessageID)

			var count int
			row := database.QueryRow("SELECT COUNT(*) FROM Requests WHERE URL = ?", r.URL)
			err := row.Scan(&count)

			if err != nil {
				log.Errorf("Fail to get count of URL: %v", err)
				continue
			}

			if count == 0 {
				log.Errorf("This URL should exist: %s", r.URL)
			}

			statement, err := database.Prepare("UPDATE Requests SET saved = 1 WHERE URL = ?")
			if err != nil {
				log.Errorf("Fail to update request to SQLite: %v", err)
				continue
			}
			_, err = statement.Exec(r.URL)
			if err != nil {
				log.Errorf("Fail to update request to SQLite: %v", err)
				continue
			}
			ackQueue <- r
		}
	}()
}

type wallabagTokenResp struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func wallabagHandler(reqQueue, diskAckQueue chan saveURLRequest) {
	bearer := ""
	bearerExpire := time.Now()

	go func() {
		for {
			var r saveURLRequest
			select {
			case r = <-reqQueue:
			case <-ctx.Done():
				return
			}
			if time.Now().After(bearerExpire) || bearer == "" {
				requestURL := fmt.Sprintf(
					"https://%s/oauth/v2/token?grant_type=password&client_id=%s&client_secret=%s&username=%s&password=%s",
					botInfo.Site,
					botInfo.ClientID,
					botInfo.ClientSecret,
					botInfo.Username,
					botInfo.Password,
				)
				resp, err := http.Get(requestURL)
				if err != nil {
					log.Errorf("Fail to get token %v", err)
					continue
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					log.Errorf("Fail to get token %+v", resp.Header)
					continue
				}

				bodyBytes, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					log.Error(err)
					continue
				}
				tokenResp := wallabagTokenResp{}
				if err = json.Unmarshal(bodyBytes, &tokenResp); err != nil {
					log.Error(err)
					continue
				}

				bearerExpire = time.Now().Local().Add(time.Second * time.Duration(tokenResp.ExpiresIn))
				bearer = tokenResp.AccessToken

				log.Infof("New token fetched: %s", bearer)
			}

			req, err := http.NewRequest(
				"POST",
				fmt.Sprintf("https://%s/api/entries.json", botInfo.Site),
				bytes.NewBuffer([]byte(fmt.Sprintf(`{ "url": "%s" }`, r.URL))))

			if err != nil {
				log.Error(err)
				continue
			}
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", bearer))

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Error(err)
				continue
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				log.Errorf("Wallabag returns error code: %v", resp.StatusCode)
				continue
			}

			log.Infof("Wallabag says it is saved: %s, %d, %d", r.URL, r.ChatID, r.MessageID)
			diskAckQueue <- r
		}

	}()
}

const timeOut = 60

func main() {
	diskQueue := make(chan saveURLRequest, 100)
	reqQueue := make(chan saveURLRequest, 100)
	diskAckQueue := make(chan saveURLRequest, 100)
	ackQueue := make(chan saveURLRequest, 100)

	wallabagHandler(reqQueue, diskAckQueue)
	sqlite3Handler(diskQueue, reqQueue, diskAckQueue, ackQueue)

	filterUsers := map[string]bool{}
	for _, s := range botInfo.FilterUsers {
		filterUsers[s] = true
	}

	bot, err := tgbotapi.NewBotAPI(botInfo.Token)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = false

	log.Infof("Authorized on account %s", bot.Self.UserName)

	go func() {
		for {
			select {
			case ackMsg := <-ackQueue:
				log.Info("Sending")
				msg := tgbotapi.NewMessage(ackMsg.ChatID, ackMsg.URL)

				bot.Send(msg)
			case <-ctx.Done():
				return
			}
		}
	}()

	rxStrict := xurls.Strict()

	offset := 0

	for {
		u := tgbotapi.NewUpdate(offset)

		updates, err := bot.GetUpdates(u)

		if err != nil {
			log.Error(err)
		}

		timer := time.NewTimer(timeOut * time.Second)

		for _, update := range updates {
			offset = 1 + update.UpdateID

			if update.Message == nil { // ignore any non-Message Updates
				continue
			}

			log.Infof("Telegram received: %s", update.Message.Text)

			if _, ok := filterUsers[update.Message.From.UserName]; !ok {
				log.Infof("Telegram discards as it is from user: %s", update.Message.From.UserName)
				continue
			}

			for _, r := range rxStrict.FindAllString(update.Message.Text, -1) {
				log.Infof("Found URL: %s", r)
				diskQueue <- saveURLRequest{URL: r, ChatID: update.Message.Chat.ID}
			}
		}

		select {
		case <-signalCh:
			cancel()
			os.Exit(0)
		case <-timer.C:
			break
		}
	}
}
