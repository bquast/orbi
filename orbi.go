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
	eventKindConfluence   = 4445
	defaultRelayTimeout   = 10 * time.Second
	localOrbiDirName      = ".orbi"
	rootEventIDFileName   = "root_event_id"
	headFileName          = "HEAD"
	trackedFilesFileName  = "tracked_files"
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
	// Filter out empty lines
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
	
	// Get existing files
	existing, err := getTrackedFiles()
	if err != nil {
		return err
	}
	
	// Check if file is already tracked
	baseFilename := filepath.Base(filename)
	for _, f := range existing {
		if f == baseFilename {
			return nil // Already tracked
		}
	}
	
	// Append new file
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

	// After successful publish, track the file
	if err := trackFile(filePath); err != nil {
		log.Printf("Warning: Failed to track file locally: %v", err)
	}
	
	return nil
}

func publishConfluence(references []string, sk, pk, message string) error {
	ev := nostr.Event{
		PubKey:    pk,
		CreatedAt: nostr.Now(),
		Kind:      eventKindConfluence,
		Content:   message,
		Tags:      make(nostr.Tags, 0),
	}

	for _, ref := range references {
		if len(ref) == 64 {
			ev.Tags = append(ev.Tags, nostr.Tag{"e", ref})
		} else {
			ev.Tags = append(ev.Tags, nostr.Tag{"f", ref})
		}
	}

	if err := ev.Sign(sk); err != nil {
		return err
	}

	fmt.Printf("Publishing confluence to relays...")
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
	if len(os.Args) < 2 {
		fmt.Println("Usage: orbi <command> [arguments]")
		fmt.Println("Commands:")
		fmt.Println("  commit <file> -m \"message\"")
		fmt.Println("  confluence <ref1> <ref2> ... -m \"message\"")
		return
	}

	switch os.Args[1] {
	case "commit":
		if len(os.Args) < 3 || os.Args[1] != "commit" {
			fmt.Println("Usage: orbi commit <file> -m \"message\"")
			return
		}

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

	case "confluence":
		if len(os.Args) < 4 {
			fmt.Println("Usage: orbi confluence <ref1> <ref2> ... -m \"message\"")
			return
		}

		args := os.Args[2:]
		var refs []string
		var message string
		
		// Parse arguments
		for i := 0; i < len(args); i++ {
			if args[i] == "-m" && i+1 < len(args) {
				message = args[i+1]
				i++
			} else {
				refs = append(refs, args[i])
			}
		}
		
		// If no refs provided, use tracked files
		if len(refs) == 0 {
			tracked, err := getTrackedFiles()
			if err != nil {
				log.Fatal("Failed to get tracked files:", err)
			}
			if len(tracked) == 0 {
				fmt.Println("No files tracked and no references specified.")
				return
			}
			refs = tracked
			fmt.Printf("Using tracked files: %v\n", refs)
		}
		
		fmt.Printf("Creating confluence with %d references and message: %s\n", len(refs), message)
		
		sk, pk, err := loadNostrSecretKey()
		if err != nil {
			log.Fatal(err)
		}
		
		err = publishConfluence(refs, sk, pk, message)
		if err != nil {
			log.Fatal(err)
		}

	default:
		fmt.Println("Unknown command. Use 'commit' or 'confluence'")
	}
}
