package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

var (
	defaultRelays = []string{"wss://relay.damus.io", "wss://relay.primal.net", "wss://nos.lol"}
)

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

func getNostrSecretPath() string {
	if path := os.Getenv(nostrSecretPathEnvVar); path != "" {
		return expandPath(path)
	}
	return expandPath(filepath.Join(defaultNostrSecretDir, defaultNostrSecretFile))
}

func getLocalOrbiFileSpecificDirPath(targetFilePath string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}
	baseFilename := filepath.Base(targetFilePath)
	safeBaseFilename := strings.ReplaceAll(baseFilename, string(os.PathSeparator), "_")

	return filepath.Join(cwd, localOrbiDirName, safeBaseFilename), nil
}

func ensureLocalOrbiFileSpecificDir(targetFilePath string) (string, error) {
	dirPath, err := getLocalOrbiFileSpecificDirPath(targetFilePath)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		fmt.Printf("Creating Orbi tracking directory for %s: %s\n", filepath.Base(targetFilePath), dirPath)
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			return "", fmt.Errorf("error creating directory %s: %w", dirPath, err)
		}
	}
	return dirPath, nil
}

func loadNostrSecretKey() (string, string, error) {
	secretPath := getNostrSecretPath()
	content, err := ioutil.ReadFile(secretPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to read secret key from %s: %w. Please ensure your secret key (nsec or hex) is stored there", secretPath, err)
	}
	skStr := strings.TrimSpace(string(content))
	if skStr == "" {
		return "", "", fmt.Errorf("secret key file %s is empty", secretPath)
	}

	var sk string
	if strings.HasPrefix(skStr, "nsec1") {
		_, decoded, err := nip19.Decode(skStr)
		if err != nil {
			return "", "", fmt.Errorf("failed to decode nsec key: %w", err)
		}
		sk = decoded.(string)
	} else if len(skStr) == 64 {
		if _, err := hex.DecodeString(skStr); err != nil {
			return "", "", fmt.Errorf("invalid hex secret key: %w", err)
		}
		sk = skStr
	} else {
		return "", "", fmt.Errorf("invalid secret key format in %s. Must be nsec or 64-char hex", secretPath)
	}

	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		return "", "", fmt.Errorf("failed to derive public key: %w", err)
	}
	npub, _ := nip19.EncodePublicKey(pk)
	fmt.Printf("Successfully loaded keys. Public key: %s\n", npub)
	return sk, pk, nil
}

func publishToRelay(ctx context.Context, relayURL string, ev nostr.Event) (bool, string) {
	relay, err := nostr.RelayConnect(ctx, relayURL)
	if err != nil {
		log.Printf("Error connecting to relay %s: %v", relayURL, err)
		return false, ""
	}
	defer relay.Close()

	publishCtx, cancel := context.WithTimeout(ctx, defaultRelayTimeout)
	defer cancel()

	err = relay.Publish(publishCtx, ev)
	if err != nil {
		log.Printf("Error publishing event %s to relay %s: %v", ev.ID, relayURL, err)
		return false, ""
	}

	log.Printf("Successfully published event %s to relay %s (assumed success as Publish returned no error)", ev.ID, relayURL)
	return true, ev.ID
}

func publishFileNostr(
	filePath string,
	sk string,
	pk string,
	relays []string,
	rootEventIDHex string,
	directParentEventIDHex string,
	commitMessage string,
) (string, []string, error) {
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	fileName := filepath.Base(filePath)

	ev := nostr.Event{
		PubKey:    pk,
		CreatedAt: nostr.Now(),
		Kind:      eventKindFile,
		Tags:      nostr.Tags{
			{"f", fileName},
		},
		Content: string(content),
	}

	if rootEventIDHex != "" {
		ev.Tags = append(ev.Tags, nostr.Tag{"e", rootEventIDHex, "", "root"})
		if directParentEventIDHex != "" {
			ev.Tags = append(ev.Tags, nostr.Tag{"e", directParentEventIDHex, "", "reply"})
		}
	}

	if commitMessage != "" {
		ev.Tags = append(ev.Tags, nostr.Tag{"m", commitMessage})
	}

	if err := ev.Sign(sk); err != nil {
		return "", nil, fmt.Errorf("failed to sign event: %w", err)
	}
	log.Printf("Built event: kind=%d, filename='%s', content_length=%d, ID: %s", ev.Kind, fileName, len(ev.Content), ev.ID)
	log.Printf("  Tags for event %s: %v", ev.ID, ev.Tags)

	var wg sync.WaitGroup
	var mu sync.Mutex
	successfulRelays := []string{}
	successCount := 0

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(relays))*defaultRelayTimeout + 5*time.Second)
	defer cancel()

	fmt.Printf("Attempting to publish event %s to %d relays...\n", ev.ID, len(relays))
	for _, rURL := range relays {
		wg.Add(1)
		go func(relayURL string) {
			defer wg.Done()
			ok, _ := publishToRelay(ctx, relayURL, ev)
			if ok {
				mu.Lock()
				successfulRelays = append(successfulRelays, relayURL)
				successCount++
				mu.Unlock()
			}
		}(rURL)
	}

	wg.Wait()

	if successCount == 0 {
		return "", nil, fmt.Errorf("failed to publish event %s to any of the specified relays", ev.ID)
	}
	fmt.Printf("Successfully published event %s to %d out of %d relays.\n", ev.ID, successCount, len(relays))
	return ev.ID, successfulRelays, nil
}

func readEventIDFromFile(targetFilePath string, idFileName string) (string, error) {
	orbiFileDir, err := getLocalOrbiFileSpecificDirPath(targetFilePath)
	if err != nil {
		return "", err
	}
	filePath := filepath.Join(orbiFileDir, idFileName)
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read %s: %w", filePath, err)
	}
	return strings.TrimSpace(string(content)), nil
}

func writeEventIDToFile(targetFilePath string, idFileName string, eventID string) error {
	orbiFileDir, err := ensureLocalOrbiFileSpecificDir(targetFilePath)
	if err != nil {
		return err
	}
	filePath := filepath.Join(orbiFileDir, idFileName)
	return ioutil.WriteFile(filePath, []byte(eventID+"\n"), 0644)
}

func main() {
	relaysFlag := flag.String("relays", strings.Join(defaultRelays, ","), "Comma-separated list of Nostr relays to publish to")
	messageFlag := flag.String("m", "", "Commit message (used with 'commit' command)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [global options] <command> [command options]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Global options:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nCommands:")
		fmt.Fprintln(os.Stderr, "  publish <filepath>                Publish a new file to Nostr (first version).")
		fmt.Fprintln(os.Stderr, "  commit <filepath> [-m message]  Commit a new version of an existing tracked file.")
		fmt.Fprintln(os.Stderr, "\nEnvironment Variables:")
		fmt.Fprintf(os.Stderr, "  %s   Path to Nostr secret key file (default: %s/%s)\n", nostrSecretPathEnvVar, defaultNostrSecretDir, defaultNostrSecretFile)
	}

	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	command := flag.Arg(0)
	if command != "publish" && command != "commit" {
		log.Printf("Error: Unknown command '%s'\n", command)
		flag.Usage()
		os.Exit(1)
	}

	if flag.NArg() < 2 {
		log.Printf("Error: Missing filepath for command '%s'\n", command)
		flag.Usage()
		os.Exit(1)
	}
	filePathArg := flag.Arg(1)
	absoluteFilePath := expandPath(filePathArg)

	if _, err := os.Stat(absoluteFilePath); os.IsNotExist(err) {
		log.Fatalf("Error: File not found at %s", absoluteFilePath)
	}

	parsedRelays := strings.Split(*relaysFlag, ",")
	if len(parsedRelays) == 0 || (len(parsedRelays) == 1 && parsedRelays[0] == "") {
		log.Println("Warning: No relays specified, using default relays.")
		parsedRelays = defaultRelays
	} else {
		for i, r := range parsedRelays {
			parsedRelays[i] = strings.TrimSpace(r)
		}
	}

	sk, pk, err := loadNostrSecretKey()
	if err != nil {
		log.Fatalf("Could not load Nostr keys: %v", err)
	}

	var currentCommitMessage string
	if command == "commit" {
		currentCommitMessage = *messageFlag
	}

	switch command {
	case "publish":
		existingRootID, err := readEventIDFromFile(absoluteFilePath, rootEventIDFileName)
		if err != nil {
			log.Fatalf("Error checking publication status for %s: %v", absoluteFilePath, err)
		}
		if existingRootID != "" {
			log.Printf("File %s has already been published. Root event ID: %s", absoluteFilePath, existingRootID)
			headID, _ := readEventIDFromFile(absoluteFilePath, headFileName)
			if headID != "" {
				log.Printf("  Current explicit HEAD is at: %s", headID)
			} else {
				log.Printf("  HEAD is implicitly the latest chronological commit.")
			}
			log.Println("Use 'orbi commit <filepath>' to publish a new version.")
			return
		}

		fmt.Printf("Publishing initial version of %s to relays: %s\n", absoluteFilePath, strings.Join(parsedRelays, ", "))
		eventID, _, err := publishFileNostr(absoluteFilePath, sk, pk, parsedRelays, "", "", "")
		if err != nil {
			log.Fatalf("Failed to publish %s: %v", absoluteFilePath, err)
		}

		if err := writeEventIDToFile(absoluteFilePath, rootEventIDFileName, eventID); err != nil {
			log.Fatalf("Failed to save root event ID for %s: %v", absoluteFilePath, err)
		}
		orbiSubDir, _ := getLocalOrbiFileSpecificDirPath(absoluteFilePath)
		fmt.Printf("Successfully published %s. Root Event ID: %s. Implicit HEAD is latest chronological. Stored in %s/\n", absoluteFilePath, eventID, orbiSubDir)

	case "commit":
		rootEventID, err := readEventIDFromFile(absoluteFilePath, rootEventIDFileName)
		if err != nil {
			log.Fatalf("Error reading root event ID for %s: %v", absoluteFilePath, err)
		}
		if rootEventID == "" {
			log.Fatalf("Error: File %s has not yet been published. Use 'orbi publish <filepath>' first.", absoluteFilePath)
		}

		parentEventIDToReply := rootEventID

		explicitHeadID, err := readEventIDFromFile(absoluteFilePath, headFileName)
		if err != nil && !os.IsNotExist(err) {
			log.Printf("Notice: Error reading explicit HEAD file for %s: %v", absoluteFilePath, err)
		}

		if explicitHeadID != "" {
			fmt.Printf("  Current explicit HEAD is set to: %s\n", explicitHeadID)
			fmt.Printf("  (This commit will still reply to root: %s as per default behavior)\n", rootEventID)
		} else {
			fmt.Printf("  No explicit HEAD set. Implicit HEAD is the latest chronological commit.\n")
		}

		fmt.Printf("Committing new version of %s to relays: %s\n", absoluteFilePath, strings.Join(parsedRelays, ", "))
		if currentCommitMessage != "" {
			fmt.Printf("  Commit message: %s\n", currentCommitMessage)
		}
		fmt.Printf("  Root event ID for this file: %s\n", rootEventID)
		fmt.Printf("  Replying directly to Root Event ID: %s\n", parentEventIDToReply)

		newEventID, _, err := publishFileNostr(absoluteFilePath, sk, pk, parsedRelays, rootEventID, parentEventIDToReply, currentCommitMessage)
		if err != nil {
			log.Fatalf("Failed to commit new version of %s: %v", absoluteFilePath, err)
		}

		orbiSubDir, _ := getLocalOrbiFileSpecificDirPath(absoluteFilePath)
		fmt.Printf("Successfully committed %s. New Event ID: %s. Stored in %s/\n", absoluteFilePath, newEventID, orbiSubDir)
		fmt.Printf("  (HEAD file was not modified by this commit operation.)\n")

	default:
		log.Fatalf("Internal error: Unhandled command %s", command)
	}
} 