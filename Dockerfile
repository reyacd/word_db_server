FROM golang:alpine as build-env

RUN mkdir /opt/word_db_server
WORKDIR /opt/word_db_server

RUN apk update
RUN apk add build-base ca-certificates git

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .

WORKDIR /opt/word_db_server/cmd/searchserver

RUN go build

RUN cd /opt/word_db_server/cmd/dbmaker && go build

# Build minimal image:
FROM alpine
COPY --from=build-env /opt/word_db_server/README.md /opt/README.md
COPY --from=build-env /opt/word_db_server/cmd/searchserver/searchserver /opt/searchserver
COPY --from=build-env /opt/word_db_server/cmd/dbmaker/dbmaker /opt/dbmaker

EXPOSE 8180

WORKDIR /opt
CMD ./searchserver