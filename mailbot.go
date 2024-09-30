package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

var hostname = flag.String("hostname", "", "tls hostname (use localhost to disable https)")
var persistPath = flag.String("persist", "persist", "directory for persistent data")
var port = flag.String("port", "https", "port to listen on")

//go:embed index.html
var indexHTML []byte

// read from $WEBHOOK_SECRET
var webhookSecret []byte

// read from $MAIL_SMTP_PASSWORD
var smtpPassword string

func main() {
	flag.Parse()
	if *hostname == "" {
		*hostname = os.Getenv("TLS_HOSTNAME")
	}
	if *hostname == "" {
		log.Fatal("please set -hostname or $TLS_HOSTNAME")
	}
	insecure := false
	if *hostname == "localhost" {
		insecure = true
		if *port == "https" {
			log.Fatal("https on localhost will not work (choose another port)")
		}
	}
	smtpPassword = os.Getenv("MAIL_SMTP_PASSWORD")
	if smtpPassword == "" {
		log.Printf("no MAIL_SMTP_PASSWORD set, will print to stdout")
	}

	if err := os.MkdirAll(*persistPath, 0770); err != nil {
		log.Fatal(err)
	}

	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("$WEBHOOK_SECRET is not set")
	}
	webhookSecret = []byte(secret)

	errorLogPath := filepath.Join(*persistPath, "errors.log")
	errorFile, err := os.OpenFile(errorLogPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0660)
	if err != nil {
		log.Fatal(err)
	}
	defer errorFile.Close()
	errorLog := log.New(errorFile, "", log.LstdFlags|log.LUTC|log.Lshortfile)

	tlsKeysDir := filepath.Join(*persistPath, "tls_keys")
	certManager := autocert.Manager{
		Cache:      autocert.DirCache(tlsKeysDir),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(*hostname, fmt.Sprintf("www.%s", *hostname)),
	}
	// This HTTP handler listens for ACME "http-01" challenges, and redirects
	// other requests. It's useful for the latter in production in case someone
	// navigates to the website without https.
	//
	// With insecure (used for localhost) this makes no sense to run.
	if !insecure {
		go func() {
			err := http.ListenAndServe(":http", certManager.HTTPHandler(nil))
			if err != nil {
				log.Fatalf("http.ListenAndServe: %s", err)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/webhook", githubEventHandler)

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%s", *port),
		Handler: mux,

		TLSConfig: &tls.Config{GetCertificate: certManager.GetCertificate},

		ErrorLog: errorLog,

		ReadTimeout:  10 * time.Second,
		WriteTimeout: 360 * time.Second,
		IdleTimeout:  360 * time.Second,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	shutdownDone := make(chan struct{})
	go func() {
		<-sigChan
		log.Printf("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := httpServer.Shutdown(ctx)
		if err != nil {
			log.Printf("HTTP server shutdown with error: %s", err)
		}
		close(shutdownDone)
	}()

	fmt.Printf("listening on :%s\n", *port)
	if insecure {
		err = httpServer.ListenAndServe()
	} else {
		err = httpServer.ListenAndServeTLS("", "")
	}
	if err != nil {
		log.Printf("http listen: %s", err)
	}

	<-shutdownDone
}

type CommitEmailConfig struct {
	MailingList string `json:"mailingList"`
	EmailFormat string `json:"emailFormat,omitempty"`
}

// getConfig reads the commit-emails.json file for a git repo
func getConfig(gitRepo string) (config CommitEmailConfig, err error) {
	configText, err := runGitCmd(gitRepo, "show", "HEAD:.github/commit-emails.json")
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(configText))
	dec.DisallowUnknownFields()
	err = dec.Decode(&config)
	if err != nil {
		return CommitEmailConfig{}, fmt.Errorf("decoding commit-emails.json: %s", err)
	}
	if config.EmailFormat != "" {
		if !(config.EmailFormat == "html" || config.EmailFormat == "text") {
			return CommitEmailConfig{}, fmt.Errorf("invalid emailFormat (should be html or text): %q", config.EmailFormat)
		}
	}
	return
}

type GithubEvent interface {
	GetRepository() GithubRepo
	GetSender() GithubSender
}

type GithubGenericEvent struct {
	Repository GithubRepo
	Sender     GithubSender
}

func (e GithubGenericEvent) GetRepository() GithubRepo { return e.Repository }
func (e GithubGenericEvent) GetSender() GithubSender   { return e.Sender }

type GithubRepo struct {
	FullName string `json:"full_name"`
	CloneUrl string `json:"clone_url"`
	Private  bool
}

type GithubSender struct {
	Login string
}

type GithubPushEvent struct {
	*GithubGenericEvent

	Ref    string
	Before string
	After  string

	Pusher struct {
		Name  string
		Email string
	}
}

func githubEventHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "unexpected method: "+req.Method, http.StatusBadRequest)
		return
	}

	sig := req.Header.Get("X-Hub-Signature-256")
	var expectedHash []byte
	if n, err := fmt.Sscanf(sig, "sha256=%x", &expectedHash); n != 1 {
		http.Error(w, "invalid signature: "+err.Error(), http.StatusBadRequest)
		return
	}

	body := http.MaxBytesReader(w, req.Body, 1024*1024)
	payload, _ := io.ReadAll(body)

	h := hmac.New(sha256.New, []byte(webhookSecret))
	h.Write(payload)
	actualHash := h.Sum(nil)
	if !hmac.Equal(actualHash, expectedHash) {
		http.Error(w, "signature verification failed", http.StatusBadRequest)
		return
	}

	eventType := req.Header.Get("X-GitHub-Event")

	var payloadData interface{}
	switch eventType {
	case "":
		http.Error(w, "no event type specified", http.StatusBadRequest)
		return
	case "push":
		payloadData = new(GithubPushEvent)
	default:
		payloadData = new(GithubGenericEvent)
	}

	if err := json.Unmarshal(payload, payloadData); err != nil {
		http.Error(w, "failed to parse payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	event := payloadData.(GithubEvent)
	log.Printf("%s: %s from %s", event.GetRepository().FullName, eventType, event.GetSender().Login)

	if eventType == "ping" {
		url := event.GetRepository().CloneUrl
		gitDir := filepath.Join(*persistPath, "repos", "github.com", event.GetRepository().FullName)

		err := syncRepo(gitDir, url)
		if err != nil {
			err = fmt.Errorf("syncing repo %q failed: %s", url, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			log.Println(err)
			return
		}

		_, _ = w.Write([]byte("Pong"))
		return
	}

	if eventType == "push" {
		pushEvent := payloadData.(*GithubPushEvent)
		err := githubPushHandler(pushEvent)
		if err != nil {
			err = fmt.Errorf("push handler failed: %s", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			log.Println(err)
			return
		}
		_, _ = w.Write([]byte("OK"))
		log.Printf("%s: push success: %s %s -> %s", pushEvent.Repository.FullName, pushEvent.Ref, pushEvent.Before[:8], pushEvent.After[:8])
	}
}

func syncRepo(gitDir string, url string) error {
	fi, err := os.Stat(gitDir)
	if os.IsNotExist(err) {
		err := gitClone(url, gitDir)
		if err != nil {
			return err
		}
		log.Printf("Cloned %s to %s", url, gitDir)
	} else if err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", gitDir)
	}

	err = gitFetch(gitDir)
	if err != nil {
		return err
	}

	return nil
}

func githubPushHandler(ev *GithubPushEvent) error {
	gitDir := filepath.Join(*persistPath, "repos", "github.com", ev.Repository.FullName)

	if err := syncRepo(gitDir, ev.Repository.CloneUrl); err != nil {
		return err
	}

	log.Printf("git_multimail_wrapper.py %s %s %s", ev.Before, ev.After, ev.Ref)
	args := []string{}
	if smtpPassword == "" {
		args = append(args, "--stdout")
	}
	config, err := getConfig(gitDir)
	if err != nil {
		log.Printf("no commit-emails.json found for %s: %s", ev.Repository.FullName, err)
		return fmt.Errorf("no commit-emails.json found for %s: %s", ev.Repository.FullName, err)
	}
	args = append(args, "-c", fmt.Sprintf("multimailhook.mailingList=%s", config.MailingList))
	if config.EmailFormat != "" {
		args = append(args, "-c", fmt.Sprintf("multimailhook.commitEmailFormat=%s", config.EmailFormat))
	}
	cmd := exec.Command("./git_multimail_wrapper.py", args...)
	stdin := bytes.NewReader([]byte(fmt.Sprintf("%s %s %s", ev.Before, ev.After, ev.Ref)))
	cmd.Stdin = stdin
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "GIT_DIR="+gitDir)
	// constants that configure git_multimail
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL="+"git-multimail.config")
	// Provide the password via an environment variable - it cannot be in the
	// config file since that's public, and we don't want it to be in the command
	// line with -c since other processes can read that.
	//
	// Single quotes are necessary for git to parse this correctly.
	cmd.Env = append(cmd.Env, "GIT_CONFIG_PARAMETERS="+fmt.Sprintf("'multimailhook.smtpPass=%s'", smtpPassword))
	_, err = cmd.Output()
	if err == nil {
		return nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Errorf("git_multimail_wrapper.py failed: %s:\n%s", ee.ProcessState.String(), ee.Stderr)
	}
	return err
}

func gitClone(url string, dest string) error {
	_, err := runGitCmd(dest, "clone", "--bare", "--quiet", url, dest)
	return err
}

func gitFetch(gitDir string) error {
	_, err := runGitCmd(gitDir, "fetch", "--quiet", "--force", "origin", "*:*")
	return err
}

func runGitCmd(gitDir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("git %v failed: %s: %q", args, ee.ProcessState.String(), ee.Stderr)
		}
	}
	return out, err
}
