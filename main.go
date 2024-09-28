package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/muesli/termenv"
	"golang.org/x/sync/semaphore"
)

const (
	depsDir     = "./"
	githubAPI   = "https://api.github.com"
	maxParallel = 5
)

type BlobResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type GithubContent struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	Sha         string `json:"sha"`
	DownloadURL string `json:"download_url"`
}

type PkgDef struct {
	Files  []string `json:"files"`
	Ignore []string `json:"ignore"`
	Branch string   `json:"branch"`
	Name   string   `json:"name"`
}

var (
	sem    = semaphore.NewWeighted(maxParallel)
	client = &http.Client{}
)

type Options struct {
	Compare bool
	Verbose bool
	Path    string
	Token   string
}

func main() {
	compare := flag.Bool("compare", false, "compare data")
	verbose := flag.Bool("verbose", false, "verbose")
	fpath := flag.String("path", "", "path")
	flag.Parse()
	value, isSet := os.LookupEnv("GITHUB_TOKEN")
	if !isSet {
		fmt.Println("Missing github token -> GITHUB_TOKEN")
		os.Exit(1)
	}
	opts := &Options{
		Compare: *compare,
		Verbose: *verbose,
		Path:    *fpath,
		Token:   value,
	}

	packageJSON, err := os.ReadFile("diffs.json")
	if err != nil {
		fmt.Println("failed to read package.json: ", err)
		os.Exit(1)
	}

	var pkg *PkgDef
	if err := json.Unmarshal(packageJSON, &pkg); err != nil {
		fmt.Println("failed to parse package.json: ", err)
		os.Exit(1)
	}
	if err := updateDependencies(opts, pkg); err != nil {
		fmt.Println("Error updating dependencies: ", err)
		os.Exit(1)
	}
}

func updateDependencies(opts *Options, pkg *PkgDef) error {
	if strings.TrimSpace(opts.Path) != "" {
		var wg sync.WaitGroup
		errs := make(chan error, 1)

		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			if err := fetchContent(dir, depsDir, opts, pkg); err != nil {
				errs <- fmt.Errorf("failed to fetch %s: %w", dir, err)
			}
		}(strings.TrimSpace(opts.Path))

		go func() {
			wg.Wait()
			close(errs)
		}()

		for err := range errs {
			if err != nil {
				return err
			}
		}
		return nil
	} else {
		var wg sync.WaitGroup
		errs := make(chan error, len(pkg.Files))

		for _, dir := range pkg.Files {
			wg.Add(1)
			go func(dir string) {
				defer wg.Done()
				if err := fetchContent(dir, depsDir, opts, pkg); err != nil {
					errs <- fmt.Errorf("failed to fetch %s: %w", dir, err)
				}
			}(dir)
		}

		go func() {
			wg.Wait()
			close(errs)
		}()

		for err := range errs {
			if err != nil {
				return err
			}
		}
		return nil
	}
}

func checkIgnore(sub string, ignore []string) bool {
	for _, v := range ignore {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return false
}

func fetchContent(path, baseDir string, opts *Options, pkgdef *PkgDef) error {
	url := fmt.Sprintf("%s/repos/%s/contents/%s", githubAPI, pkgdef.Name, path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "token "+opts.Token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d -> %s", resp.StatusCode, path)
	}

	var contents []GithubContent
	if err := json.NewDecoder(resp.Body).Decode(&contents); err != nil {
		resp.Body.Close()
		resp, err = client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to make request: %w", err)
		}
		defer resp.Body.Close()
		var content GithubContent
		if err := json.NewDecoder(resp.Body).Decode(&content); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
		contents = []GithubContent{content}
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(contents))

	for _, content := range contents {
		if !checkIgnore(content.Path, pkgdef.Ignore) {
			wg.Add(1)
			go func(content GithubContent) {
				defer wg.Done()
				switch content.Type {
				case "dir":
					if err := fetchContent(content.Path, baseDir, opts, pkgdef); err != nil {
						errs <- err
					}
				case "file":
					if err := downloadFile(content.DownloadURL, filepath.Join(baseDir, content.Path), opts, content.Sha, pkgdef); err != nil {
						errs <- fmt.Errorf("failed to download %s: %w", content.Path, err)
					} else {
						if !opts.Compare {
							fmt.Printf("Fetched file: %s\n", content.Path)
						}
					}
				}
			}(content)
		}
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

func getContentGitSha(sha string, token string, pkgdef *PkgDef) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/git/blobs/%s", githubAPI, pkgdef.Name, sha)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var blobResp BlobResponse
	err = json.Unmarshal(body, &blobResp)
	if err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(blobResp.Content)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func getFileContentBySHA(sha string) (string, error) {
	cmd := exec.Command("git", "cat-file", "-p", sha)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to retrieve file content: %v, output: %s", err, output)
	}
	return string(output), nil
}

func diffFilesInMemory(content1, content2 string) string {
	var diffBuilder strings.Builder
	lines1 := strings.Split(strings.TrimSpace(content1), "\n")
	lines2 := strings.Split(strings.TrimSpace(content2), "\n")

	maxLen := len(lines1)
	if len(lines2) > maxLen {
		maxLen = len(lines2)
	}

	for i := 0; i < maxLen; i++ {
		line1 := ""
		line2 := ""
		if i < len(lines1) {
			line1 = strings.TrimSpace(lines1[i])
		}
		if i < len(lines2) {
			line2 = strings.TrimSpace(lines2[i])
		}

		if line1 != line2 {
			if line1 != "" {
				diffBuilder.WriteString(fmt.Sprintf("-%s\n", line1))
			}
			if line2 != "" {
				diffBuilder.WriteString(fmt.Sprintf("+%s\n", line2))
			}
		}
	}
	return diffBuilder.String()
}

func countDiffLines(diff string) (int, error) {
	scanner := bufio.NewScanner(strings.NewReader(diff))
	count := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			count++
		}
	}
	return count, scanner.Err()
}

func calculateLocalSHA(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	header_size := fmt.Sprintf("blob %d\x00", info.Size())
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	store := append([]byte(header_size), content...)
	hash_store := sha1.New()
	hash_store.Write(store)

	return hex.EncodeToString(hash_store.Sum(nil)), nil
}

func downloadFile(url, filePath string, opts *Options, gitsha string, pkgdef *PkgDef) error {
	ctx := context.Background()
	if err := sem.Acquire(ctx, 1); err != nil {
		return fmt.Errorf("failed to acquire semaphore: %w", err)
	}
	defer sem.Release(1)

	if opts.Compare && opts.Verbose {
		if _, err := os.Stat(filePath); !os.IsNotExist(err) {
			localsha, err := calculateLocalSHA(filePath)
			if err != nil {
				return err
			}
			if localsha != gitsha {
				shalocal, err := getFileContentBySHA(localsha)
				if err != nil {
					log.Println("error in shalocal")
					return err
				}
				shagit, err := getContentGitSha(gitsha, opts.Token, pkgdef)
				if err != nil {
					log.Println("error in shagit")
					return err
				}
				diff := diffFilesInMemory(shalocal, shagit)
				totalDiffs, err := countDiffLines(diff)
				if err != nil {
					log.Println("error in diff")
					return err
				}
				var markdownBuilder strings.Builder
				log.Printf("%d Differences for: %s\n", totalDiffs, filePath)
				lines := strings.Split(diff, "\n")
				markdownBuilder.WriteString("```diff\n")
				for _, line := range lines {
					if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+") {
						markdownBuilder.WriteString(fmt.Sprintf("%s\n", line))
					}
				}
				markdownBuilder.WriteString("```\n")
				r, _ := glamour.NewTermRenderer(
					glamour.WithStandardStyle("dark"),
					glamour.WithWordWrap(80),
					glamour.WithColorProfile(termenv.Profile(0)),
				)
				out, err := r.Render(markdownBuilder.String())
				if err != nil {
					fmt.Printf("Error rendering: %v\n", err)
					return err
				}

				fmt.Print(out)
			}
		}
	} else if opts.Compare && !opts.Verbose {
		if _, err := os.Stat(filePath); !os.IsNotExist(err) {
			localsha, err := calculateLocalSHA(filePath)
			if err != nil {
				return err
			}
			if localsha != gitsha {
				shalocal, err := getFileContentBySHA(localsha)
				if err != nil {
					log.Println("error in shalocal")
					return err
				}
				shagit, err := getContentGitSha(gitsha, opts.Token, pkgdef)
				if err != nil {
					log.Println("error in shagit")
					return err
				}
				diff := diffFilesInMemory(shalocal, shagit)
				totalDiffs, err := countDiffLines(diff)
				if err != nil {
					log.Println("error in diff")
					return err
				}
				log.Printf("%d Differences for: %s\n", totalDiffs, filePath)
			}
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}

		resp, err := http.Get(url)
		if err != nil {
			return fmt.Errorf("failed to download file: %w", err)
		}
		defer resp.Body.Close()

		out, err := os.Create(filePath)
		if err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		defer out.Close()

		_, err = io.Copy(out, resp.Body)
		return err
	}
	return nil
}
