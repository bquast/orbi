package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const (
	nostrSecretPathEnvVar  = "NOSTR_SECRET_PATH"
	defaultNostrSecretDir  = "~/.nostr"
	defaultNostrSecretFile = "secret"
	eventKindFile          = 4444
	defaultRelayTimeout    = 10 * time.Second
	localOrbiDirName       = ".orbi"
	trackedFilesFileName   = "tracked_files"
)

var defaultRelays = []string{
	"wss://relay.damus.io",
	"wss://relay.primal.net",
	"wss://nos.lol",
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("Error getting user home directory: %v", err)
		}
		path = filepath.Join(home, path[2:])
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		log.Fatalf("Error getting absolute path for %s: %v", path, err)
	}
	return absPath
}

func loadNostrSecretKey() (string, string, error) {
	secretPath := expandPath(filepath.Join(defaultNostrSecretDir, defaultNostrSecretFile))
	if envPath := os.Getenv(nostrSecretPathEnvVar); envPath != "" {
		secretPath = expandPath(envPath)
	}
	content, err := ioutil.ReadFile(secretPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read secret key: %w", err)
	}
	skStr := strings.TrimSpace(string(content))
	var sk string
	if strings.HasPrefix(skStr, "nsec1") {
		_, decoded, err := nip19.Decode(skStr)
		if err != nil {
			return "", "", err
		}
		sk = decoded.(string)
	} else if len(skStr) == 64 {
		if _, err := hex.DecodeString(skStr); err != nil {
			return "", "", err
		}
		sk = skStr
	} else {
		return "", "", fmt.Errorf("invalid key format")
	}
	pk, _ := nostr.GetPublicKey(sk)
	return sk, pk, nil
}

func getTrackedFiles() ([]string, error) {
	orbiDir := filepath.Join(".", localOrbiDirName)
	trackedFilesPath := filepath.Join(orbiDir, trackedFilesFileName)

	if _, err := os.Stat(trackedFilesPath); os.IsNotExist(err) {
		return []string{}, nil
	}

	content, err := ioutil.ReadFile(trackedFilesPath)
	if err != nil {
		return nil, err
	}

	files := strings.Split(strings.TrimSpace(string(content)), "\n")
	var result []string
	for _, f := range files {
		if f != "" {
			result = append(result, f)
		}
	}
	return result, nil
}

func trackFile(filename string) error {
	orbiDir := filepath.Join(".", localOrbiDirName)
	if err := os.MkdirAll(orbiDir, 0755); err != nil {
		return err
	}

	trackedFilesPath := filepath.Join(orbiDir, trackedFilesFileName)
	existing, err := getTrackedFiles()
	if err != nil {
		return err
	}

	baseFilename := filepath.Base(filename)
	for _, f := range existing {
		if f == baseFilename {
			return nil // Already tracked
		}
	}

	f, err := os.OpenFile(trackedFilesPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.WriteString(baseFilename + "\n"); err != nil {
		return err
	}
	return nil
}

func publishFile(filePath, sk, pk, message string) error {
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		return err
	}

	ev := nostr.Event{
		PubKey:    pk,
		CreatedAt: nostr.Now(),
		Kind:      eventKindFile,
		Content:   string(content),
		Tags:      nostr.Tags{{"f", filepath.Base(filePath)}},
	}
	if message != "" {
		ev.Tags = append(ev.Tags, nostr.Tag{"m", message})
	}
	if err := ev.Sign(sk); err != nil {
		return err
	}

	fmt.Println("Publishing file to relays...")
	for _, r := range defaultRelays {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRelayTimeout)
		defer cancel()
		relay, err := nostr.RelayConnect(ctx, r)
		if err != nil {
			log.Printf("Failed to connect to %s: %v", r, err)
			continue
		}
		relay.Publish(ctx, ev)
		relay.Close()
		log.Printf("Published to %s", r)
	}

	if err := trackFile(filePath); err != nil {
		log.Printf("Warning: Failed to track file locally: %v", err)
	}

	fmt.Printf("\nSuccessfully published file %s\nEvent ID: %s\n", filepath.Base(filePath), ev.ID)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: orbi <file> [message]")
		return
	}

	file := os.Args[1]
	var message string
	if len(os.Args) > 2 {
		message = os.Args[2]
	}

	fmt.Printf("Committing %s with message: \"%s\"\n", file, message)
	file = expandPath(file)

	sk, pk, err := loadNostrSecretKey()
	if err != nil {
		log.Fatal(err)
	}

	err = publishFile(file, sk, pk, message)
	if err != nil {
		log.Fatal(err)
	}
}