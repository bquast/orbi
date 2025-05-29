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
	nostrSecretPathEnvVar = "NOSTR_SECRET_PATH"
	defaultNostrSecretDir = "~/.nostr"
	defaultNostrSecretFile = "secret"
	eventKindFile         = 4444
	defaultRelayTimeout   = 10 * time.Second
	localOrbiDirName      = ".orbi"
	rootEventIDFileName   = "root_event_id"
	headFileName          = "HEAD"
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

	fmt.Printf("Publishing to relays...")
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
	return nil
}

func main() {
	if len(os.Args) < 3 || os.Args[1] != "commit" {
		fmt.Println("Usage: orbi commit <file> -m \"message\"")
		return
	}

	// parse manually
	args := os.Args[2:]
	var file string
	var message string
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" && i+1 < len(args) {
			message = args[i+1]
			i++
		} else if file == "" {
			file = args[i]
		}
	}

	if file == "" {
		fmt.Println("No file specified.")
		return
	}

	fmt.Printf("Committing %s with message: %s", file, message)
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
