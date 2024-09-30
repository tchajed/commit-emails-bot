# syntax=docker/dockerfile:1
# https://www.docker.com/blog/containerize-your-go-developer-environment-part-2/
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY mailbot.go index.html ./
RUN go build -o commit-email-bot

FROM python:3.12-slim
WORKDIR /app
RUN set -eux; \
  apt-get update; \
  apt-get install -y --no-install-recommends git; \
  apt-get clean; \
  rm -rf /var/lib/apt/lists/*

# Copy the Go binary built from the build stage
COPY --from=build /src/commit-email-bot .
COPY post-receive.py requirements.txt ./
RUN pip3 install -r requirements.txt

EXPOSE 8888
ENV TLS_HOSTNAME="commit-emails.xyz"
CMD [ "/app/commit-email-bot", "-port", "8888" ]
