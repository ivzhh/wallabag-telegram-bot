FROM golang:1.13-buster AS build-env
RUN apt install git gcc
RUN go get github.com/mattn/go-sqlite3 \
    && go get github.com/go-telegram-bot-api/telegram-bot-api \
    && go get mvdan.cc/xurls \
    && go get github.com/sirupsen/logrus 
ADD . /src
RUN cd /src && go build -o wallabag-bot

# final stage
FROM golang:1.13-buster
WORKDIR /root/.config/t.me
COPY --from=build-env /src/wallabag-bot /app/
ENTRYPOINT /app/wallabag-bot
