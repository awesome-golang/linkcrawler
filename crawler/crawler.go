package crawler

import (
	"bytes"
	"compress/gzip"
	"encoding/base32"
	"io/ioutil"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goware/urlx"
	"github.com/jackdanger/collectlinks"
	"github.com/schollz/jsonstore"
)

// Crawler is the crawler instance
type Crawler struct {
	client               *http.Client
	wg                   sync.WaitGroup
	done                 *jsonstore.JSONStore
	todo                 *jsonstore.JSONStore
	trash                *jsonstore.JSONStore
	programTime          time.Time
	curFileList          map[string]bool
	numberOfURLSParsed   int
	BaseURL              string
	KeywordsToExclude    []string
	KeywordsToInclude    []string
	MaxNumberWorkers     int
	MaxNumberConnections int
	Verbose              bool
	FilePrefix           string
}

func encodeURL(url string) string {
	return base32.StdEncoding.EncodeToString([]byte(url))
}

// New will create a new crawler
func New(url string) (*Crawler, error) {
	c := new(Crawler)
	c.BaseURL = url
	c.MaxNumberConnections = 100
	c.MaxNumberWorkers = 100
	c.FilePrefix = encodeURL(url)
	return c, nil
}

func (c *Crawler) loadKeyStores() error {
	var err, err2 error

	// Initialize the keystores
	c.done = new(jsonstore.JSONStore)
	c.todo = new(jsonstore.JSONStore)
	c.trash = new(jsonstore.JSONStore)

	// Load the keystores if they already exist
	if _, err = os.Stat(c.FilePrefix + "_todo.json"); err == nil {
		c.todo, err2 = jsonstore.Open(c.FilePrefix + "_todo.json")
		if err2 != nil {
			return err2
		}
	} else {
		c.todo.Set(c.BaseURL, true)
	}

	if _, err = os.Stat(c.FilePrefix + "_done.json"); err == nil {
		c.done, err2 = jsonstore.Open(c.FilePrefix + "_done.json")
		if err2 != nil {
			return err2
		}
	}
	if _, err = os.Stat(c.FilePrefix + "_trash.json"); err == nil {
		c.trash, err2 = jsonstore.Open(c.FilePrefix + "_trash.json")
		if err2 != nil {
			return err2
		}
	}
	return nil
}

// saveData saves the data to the respective files
func (c *Crawler) saveKeyStores() (int, error) {
	err1 := jsonstore.Save(c.done, c.FilePrefix+"_done.json")
	err2 := jsonstore.Save(c.todo, c.FilePrefix+"_todo.json")
	err3 := jsonstore.Save(c.trash, c.FilePrefix+"_trash.json")
	if err1 != nil {
		return -1, err1
	} else if err2 != nil {
		return -1, err2
	} else if err3 != nil {
		return -1, err3
	}
	parsingTime := time.Since(c.programTime)
	timePerURL := parsingTime.Seconds() / float64(c.numberOfURLSParsed)
	numTodo := len(c.todo.GetAll(regexp.MustCompile(`.*`)))
	numFinished := len(c.done.GetAll(regexp.MustCompile(`.*`)))
	log.Printf("Parsed %d urls in %s (%2.5f seconds / URL), %d/%d\n", c.numberOfURLSParsed, parsingTime.String(), timePerURL, numFinished, numTodo)
	return numTodo, nil
}

func (c *Crawler) collectLinks(url string, download bool) error {
	// Decrement the counter when the goroutine completes.
	defer c.wg.Done()

	if download {
		// Check if it is already downloaded
		if _, ok := c.curFileList[encodeURL(url)]; ok {
			if c.Verbose {
				log.Printf("Already downloaded %s", url)
			}
			c.todo.Delete(url)
			c.done.Set(url, 0)
			return nil
		}
	}

	var foo, currentNumberOfTries int

	// Check if it is done
	if c.done.Get(url, &foo) == nil {
		c.todo.Delete(url)
		return nil
	}

	c.todo.Get(url, &currentNumberOfTries)
	currentNumberOfTries++

	resp, err := c.client.Get(url)
	if err != nil {
		c.trash.Set(url, currentNumberOfTries)
		c.todo.Delete(url)
		if c.Verbose {
			log.Printf("Problem with %s: %s", url, err.Error())
		}
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		c.numberOfURLSParsed++

		// Download, if downloading
		if download {
			contentType := resp.Header.Get("Content-type")
			contentTypes, contentTypeErr := mime.ExtensionsByType(contentType)
			extension := ""
			if contentTypeErr == nil {
				extension = contentTypes[0]
				if extension == ".htm" || extension == ".hxa" {
					extension = ".html"
				}
			} else {
				return err
			}
			file_content, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			var buf bytes.Buffer
			writer := gzip.NewWriter(&buf)
			writer.Write(file_content)
			writer.Close()
			filename := encodeURL(url) + extension + ".gz"
			os.Mkdir("downloaded", 0755)
			err = ioutil.WriteFile(path.Join("downloaded", filename), buf.Bytes(), 0755)
			if err != nil {
				return err
			}

			if c.Verbose {
				log.Printf("Saved %s to %s", url, encodeURL(url)+extension)
			}
		} else {
			links := collectlinks.All(resp.Body)
			if c.Verbose {
				log.Printf("Got %d links from %s\n", len(links), url)
			}
			for _, link := range links {
				// Do not use query parameters
				if strings.Contains(link, "?") {
					link = strings.Split(link, "?")[0]
				}
				// Add the Base URL to everything if it doesn't have it
				if !strings.Contains(link, "http") {
					link = c.BaseURL + link
				}
				// Skip links that have a different Base URL
				if !strings.Contains(link, c.BaseURL) {
					if c.Verbose {
						log.Printf("Skipping %s because it has a different base URL", link)
					}
					continue
				}
				// Normalize the link
				parsedLink, _ := urlx.Parse(link)
				normalizedLink, _ := urlx.Normalize(parsedLink)
				if len(normalizedLink) == 0 {
					continue
				}

				// Exclude keywords, skip if any are found
				foundExcludedKeyword := false
				for _, keyword := range c.KeywordsToExclude {
					if strings.Contains(normalizedLink, keyword) {
						foundExcludedKeyword = true
						if c.Verbose {
							log.Printf("Skipping %s because contains %s", link, keyword)
						}
						break
					}
				}
				if foundExcludedKeyword {
					continue
				}

				// Include keywords, skip if any are NOT found
				foundIncludedKeyword := false
				for _, keyword := range c.KeywordsToInclude {
					if strings.Contains(normalizedLink, keyword) {
						foundIncludedKeyword = true
						break
					}
				}
				if !foundIncludedKeyword && len(c.KeywordsToInclude) > 0 {
					continue
				}

				// Add the new link if its not queued anywhere else
				if c.todo.Get(normalizedLink, &foo) != nil &&
					c.done.Get(normalizedLink, &foo) != nil &&
					c.trash.Get(normalizedLink, &foo) != nil {
					c.todo.Set(normalizedLink, 0)
				}
			}
		}

		// Dequeue the current URL
		c.done.Set(url, currentNumberOfTries)
		c.todo.Delete(url)
	} else {
		if currentNumberOfTries > 3 {
			// Delete this URL as it has been tried too many times
			c.trash.Set(url, currentNumberOfTries)
			c.todo.Delete(url)
			if c.Verbose {
				log.Println("Too many tries, deleting " + url)
			}
		} else {
			c.todo.Set(url, currentNumberOfTries)
		}
	}
	return nil
}

// Get a list of the done urls
func (c *Crawler) GetLinks() []string {
	doneMap := c.done.GetAll(regexp.MustCompile(`.*`))
	todoMap := c.todo.GetAll(regexp.MustCompile(`.*`))
	allLinks := make([]string, len(doneMap)+len(todoMap))
	i := 0
	for link := range doneMap {
		allLinks[i] = link
		i++
	}
	for link := range todoMap {
		allLinks[i] = link
		i++
	}
	return allLinks
}

// Crawl downloads the pages specified in the todo file
func (c *Crawler) Download(urls []string) error {
	// Initialize the keystores
	c.done = new(jsonstore.JSONStore)
	c.todo = new(jsonstore.JSONStore)
	c.trash = new(jsonstore.JSONStore)

	// Generate the todo set
	for _, url := range urls {
		c.todo.Set(url, 0)
	}

	// Determine which files have been downloaded
	c.curFileList = make(map[string]bool)
	files, err := ioutil.ReadDir("downloaded")
	if err == nil {
		for _, f := range files {
			name := strings.Split(f.Name(), ".")[0]
			if len(name) < 2 {
				continue
			}
			c.curFileList[name] = true
		}
	}
	download := true
	return c.downloadOrCrawl(download)
}

// Crawl is the function to crawl with the set parameters
func (c *Crawler) Crawl() error {
	// Initialize/load the key stores
	err := c.loadKeyStores()
	if err != nil {
		return err
	}

	download := false
	return c.downloadOrCrawl(download)
}

func (c *Crawler) downloadOrCrawl(download bool) error {
	// Generate the connection pool
	tr := &http.Transport{
		MaxIdleConns:       c.MaxNumberConnections,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
	c.client = &http.Client{Transport: tr}

	c.programTime = time.Now()
	c.numberOfURLSParsed = 0
	it := 0
	for {
		it++
		numWorkers := 0
		for url := range c.todo.GetAll(regexp.MustCompile(`.*`)) {
			numWorkers++
			if numWorkers > c.MaxNumberWorkers {
				break
			}
			c.wg.Add(1)
			go c.collectLinks(url, download)
		}
		c.wg.Wait()

		// Save every 5th iteration
		if math.Mod(float64(it), float64(5)) == 0 {
			numTodo, err2 := c.saveKeyStores()
			if err2 != nil || numTodo == 0 {
				return err2
			}
		}
	}
	return nil
}