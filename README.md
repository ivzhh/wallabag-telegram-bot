# Usage

## Prepare Configuration File

Please put a json file of the following format in path `$HOME/.config/t.me/wallabag.json`

```json
{
    "token": "wallabag-token",
    "wallabag_site": "wallabag.yourdomain.com",
    "client_id": "wallabag-client-id",
    "client_secret": "wallabag-client-secret",
    "username": "wallabag-username",
    "password": "wallabag-password"
}
```

## Install Dependencies

```sh
go get github.com/mattn/go-sqlite3 \
    github.com/go-telegram-bot-api/telegram-bot-api \
    mvdan.cc/xurls \
    github.com/sirupsen/logrus
```

## Run The Bot

```go
go run main.go
```

## Usage 2: Docker + Systemd

```sh
sudo docker build . -t localhost/wallabag-bot

sudo mkdir -p /etc/wallabag-bot
# fix the path in docker-compose.yaml, then
sudo cp docker-compose.yaml /etc/wallabag-bot
sudo cp docker-service@.service /etc/systemd/system/

sudo systemctl enable --now docker-service@wallabag-bot.service
```

## Ask questions
[@]ivz hh](https://github.com/ivzhh)
