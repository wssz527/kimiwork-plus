package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type manifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	Version       string             `json:"version"`
	Channel       string             `json:"channel"`
	Name          string             `json:"name,omitempty"`
	Notes         string             `json:"notes,omitempty"`
	PublishedAt   string             `json:"publishedAt"`
	Artifacts     []manifestArtifact `json:"artifacts"`
}

type manifestArtifact struct {
	URL           string `json:"url"`
	Filename      string `json:"filename"`
	Filetype      string `json:"filetype,omitempty"`
	Size          int64  `json:"size"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch,omitempty"`
	DigestAlgo    string `json:"digestAlgo"`
	Digest        string `json:"digest"`
	SignatureAlgo string `json:"signatureAlgo"`
	Signature     string `json:"signature"`
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	genKeyDir := flag.String("gen-key", "", "generate a new ed25519 release signing keypair into this directory")
	privKeyPath := flag.String("key", "", "path to ed25519 private key file (hex or base64)")
	version := flag.String("version", "", "release semver (without v prefix, e.g. 1.0.1)")
	channel := flag.String("channel", "stable", "release channel (stable or preview)")
	name := flag.String("name", "", "release display title")
	notes := flag.String("notes", "", "release notes text")
	notesFile := flag.String("notes-file", "", "path to file containing release notes")
	baseURL := flag.String("base-url", "", "optional base URL prepended to artifact filenames")
	outPath := flag.String("out", "update-manifest.json", "output manifest JSON path")
	var artifacts stringList
	flag.Var(&artifacts, "artifact", "artifact spec platform:arch:filepath[:url] (repeatable)")
	flag.Parse()

	if *genKeyDir != "" {
		if err := generateKeypair(*genKeyDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *privKeyPath == "" || *version == "" || len(artifacts) == 0 {
		fmt.Fprintln(os.Stderr, "usage: signrelease -gen-key <dir> | -key <privkey> -version <semver> [-channel stable|preview] [-notes ...] [-base-url ...] -artifact platform:arch:filepath[:url] [-out update-manifest.json]")
		os.Exit(2)
	}

	priv, err := loadPrivateKey(*privKeyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	releaseNotes := *notes
	if *notesFile != "" {
		raw, err := os.ReadFile(*notesFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		releaseNotes = strings.TrimSpace(string(raw))
	}

	m := manifest{
		SchemaVersion: 1,
		Version:       strings.TrimPrefix(strings.TrimSpace(*version), "v"),
		Channel:       strings.TrimSpace(*channel),
		Name:          strings.TrimSpace(*name),
		Notes:         releaseNotes,
		PublishedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if m.Name == "" {
		m.Name = "Kimi Work+ " + m.Version
	}

	for _, spec := range artifacts {
		art, sigB64, err := signArtifactSpec(priv, spec, *baseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		m.Artifacts = append(m.Artifacts, art)
		sigPath := filepath.Join(filepath.Dir(*outPath), art.Filename+".sig")
		if err := os.WriteFile(sigPath, []byte(sigB64+"\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*outPath, append(raw, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Signed manifest %s (%d artifacts)\n", *outPath, len(m.Artifacts))
}

func generateKeypair(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	privPath := filepath.Join(dir, "ed25519.key")
	pubPath := filepath.Join(dir, "ed25519.pub")
	if _, err := os.Stat(privPath); err == nil {
		return fmt.Errorf("private key already exists at %s; refusing to overwrite", privPath)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(privPath, []byte(hex.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, []byte(hex.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("Generated Ed25519 update signing keypair:\n  Public (hex): %s\n  Public file:  %s\n  Private file: %s\n", hex.EncodeToString(pub), pubPath, privPath)
	return nil
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(raw))
	if b, err := hex.DecodeString(text); err == nil && len(b) == ed25519.PrivateKeySize {
		return ed25519.PrivateKey(b), nil
	}
	if b, err := base64.StdEncoding.DecodeString(text); err == nil && len(b) == ed25519.PrivateKeySize {
		return ed25519.PrivateKey(b), nil
	}
	return nil, fmt.Errorf("invalid ed25519 private key in %s", path)
}

func signArtifactSpec(priv ed25519.PrivateKey, spec, baseURL string) (manifestArtifact, string, error) {
	// Format: platform:arch:filepath[:url] (note Windows paths are unlikely here; split max 4 with care for http(s)://)
	first := strings.IndexByte(spec, ':')
	if first < 0 {
		return manifestArtifact{}, "", fmt.Errorf("invalid artifact spec %q", spec)
	}
	platformName := spec[:first]
	rest := spec[first+1:]
	second := strings.IndexByte(rest, ':')
	if second < 0 {
		return manifestArtifact{}, "", fmt.Errorf("invalid artifact spec %q", spec)
	}
	archName := rest[:second]
	pathAndURL := rest[second+1:]

	filePath := pathAndURL
	artURL := ""
	if idx := strings.Index(pathAndURL, ":http://"); idx >= 0 {
		filePath = pathAndURL[:idx]
		artURL = pathAndURL[idx+1:]
	} else if idx := strings.Index(pathAndURL, ":https://"); idx >= 0 {
		filePath = pathAndURL[:idx]
		artURL = pathAndURL[idx+1:]
	}

	f, err := os.Open(filePath)
	if err != nil {
		return manifestArtifact{}, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return manifestArtifact{}, "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return manifestArtifact{}, "", err
	}
	digest := h.Sum(nil)
	sig := ed25519.Sign(priv, digest)

	filename := filepath.Base(filePath)
	if artURL == "" {
		if baseURL != "" {
			artURL = strings.TrimRight(baseURL, "/") + "/" + filename
		} else {
			artURL = filename
		}
	}
	filetype := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	return manifestArtifact{
		URL:           artURL,
		Filename:      filename,
		Filetype:      filetype,
		Size:          info.Size(),
		Platform:      platformName,
		Arch:          archName,
		DigestAlgo:    "sha256",
		Digest:        base64.StdEncoding.EncodeToString(digest),
		SignatureAlgo: "ed25519",
		Signature:     sigB64,
	}, sigB64, nil
}
