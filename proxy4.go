package main

import (
	"compress/gzip" // uncomment this line to use the gzip package
	"flag"
	"fmt"
	"golang.org/x/net/html" // uncomment this line to use the html package
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
	"strings"
	"bufio"
	"container/list" // this is for the list..
	"io"
	"strconv" 
	"io/ioutil"
	"bytes"
)

const EXIT_FAILURE = 1
const Server_Error = "<html><head>\r\b<title>500 Internal Server Error</title>\r\n</head><body>\r\n<h1>Internal Server Error</h1>\r\n</body></html>\r\n"
const Server_Message = "HTTP/1.1 500 Internal Server Error\r\n" + "Connection: close\r\n" + "Content-Length: 129\r\n" + "Content-Type: text/html\r\n\r\n" + Server_Error + "\r\n"


// Global constants
const (
	SERVERROR	= 500
	BADREQ		= 400
	MAX_OBJ_SIZE	= 500*1024
	MAX_CACHE_SIZE	= 10*1024*1024
)

// Command line parameters
var (
	listeningPort	uint
	dnsPrefetching	bool
	caching		bool
	cacheTimeout	uint
	maxCacheSize	uint
	maxObjSize	uint
	linkPrefetching	bool
	maxConcurrency	uint
	outputFile	string
)

// Channel to synchronize number of prefetch threads
var semConc chan bool

// structure for the cacheItem, what we will be caching
type node struct {
	url string
	response *http.Response
	cacheTime time.Time
}

// Data structures for the LRU cache implementation
var (
	cacheList *list.List
	cacheMap map[string]*list.Element
 	cacheSize uint
)

// Stat variables
var (
	clientRequests	int	// HTTP requests
	cacheHits	int	// Cache Hits
	cacheMisses	int	// Cache misses
	cacheEvictions	int	// Cache evictions
	trafficSent	int	// Bytes sent to clients
	volumeFromCache	int	// Bytes sent from the cache
	downloadVolume	int	// Bytes downloaded from servers
)

// RW lock for the stat variables. 
// You need to lock the stat variables when updating them.
var statLock	*sync.RWMutex
var cacheLock   *sync.Mutex

func getPath(url string) string {
	urlArray := strings.Split(url, "/")
	path := "/"
	if len(urlArray) == 3 { 
		return path
	} else {
		for i := 3; i < len(urlArray); i++ {
			path = path + urlArray[i] + "/"
		}
		return path
	}
}
func getHost(url string) string {
	urlArray := strings.Split(url, "/")
	return urlArray[2]
}
func saveStatistics() {
	f, err := os.Create(outputFile)
	if err != nil {
		log.Fatal("Error creating output file", outputFile)
	}
	start := time.Now()
	str := "#Time(s)\tclientRequests\tcacheHits\tcacheMisses\tcacheEvictions" +
		"\ttrafficSent\tvolumeFromCache\tdownloadVolume\ttrafficWastage\tcacheEfficiency";
	f.WriteString(str)
	for {
		var cacheEfficiency	float64
		var trafficWastage	int
		
		currentTime := time.Now().Sub(start)
		statLock.RLock()
		if trafficSent > 0 {
			cacheEfficiency = float64(volumeFromCache) / float64(trafficSent)
		} else {
			cacheEfficiency = 0.0
		}
		if downloadVolume > trafficSent {
			trafficWastage = downloadVolume-trafficSent
		} else {
			trafficWastage = 0;
		}
		
		str := fmt.Sprintf("\n%d\t\t%d\t\t%d\t\t%d\t\t%d\t\t%d\t\t%d\t\t%d\t\t%d\t\t%f",
			int(currentTime.Seconds()),	clientRequests,
			cacheHits, cacheMisses, cacheEvictions,
			trafficSent, volumeFromCache, downloadVolume,
			trafficWastage, cacheEfficiency)
		statLock.RUnlock()
		f.WriteString(str)
		f.Sync()
		time.Sleep(time.Second * 10)		
	}
}

func stringInArray(key string, list []string) bool {
    for _, b := range list {
        if strings.ToLower(b) == strings.ToLower(key) {
            return true
        }
    }
    return false
}


func debugCache() {
	fmt.Printf("length of cache list is: %d\n", cacheList.Len())
	fmt.Printf("length of cache map is: %d\n", len(cacheMap))
	
	for e := cacheList.Front(); e != nil; e = e.Next() {
	// do something with e.Value
		fmt.Printf("%d\n", e.Value.(*node).url)
	}
	
}

func parse (n *html.Node) []string {
	array := []string{}
	if (n.Type == html.ElementNode && n.Data == "a") {
		for _, a := range n.Attr {
			if a.Key == "href" {
				if (!strings.Contains(a.Val, "https://") && !strings.Contains(a.Val, "ftp://")) {
					if (strings.Index(a.Val, "http://") == 0 || strings.Index(a.Val, "/") == 0) {
						array = append(array, a.Val)
					}
				}
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		array = append(array, parse(c)...)
	}
	return array
}

func handleUrl(url string, host string, channels chan bool) {
	var urlString string
	if (strings.Contains(url, "http://")) {
		urlString = url
	} else {
		urlString = "http://" + host + url 
	}
	if (!strings.HasSuffix(urlString, "/")) {
		urlString += "/"
	}

	host = getHost(urlString)
	// fmt.Println(host)

	ipQuery := host + ":80"

	conn, err := net.Dial("tcp", ipQuery)
	if err != nil {
		fmt.Println("error in net.Dial")
		<-channels
		return
	}
	defer conn.Close()

	if val, ok := cacheMap[urlString]; ok {
		currentTime := time.Now()
		cacheNode := val.Value.(*node)
		diff := currentTime.Sub(cacheNode.cacheTime)

		if (uint(diff.Seconds()) < cacheTimeout) {
		   	//move node to front of list
		   	// TODO: double check with cody about this
		   	cacheLock.Lock()
		   	cacheList.MoveToFront(val)
		   	cacheLock.Unlock()
		   	statLock.Lock()
		   	cacheHits++
		   	statLock.Unlock()

		    <-channels
		    // fmt.Println("is it returning here?")
		    return
		} else {
		   	//conditional GET
		    //add to header If-Modified-Since
		    modifiedTime := cacheNode.cacheTime.UTC()
		    timeString := modifiedTime.Format("Mon, 02 Jan 2006 15:04:05 GMT")
		    
		    // conn.Write([]byte("GET " + getPath(urlString) + " HTTP/1.1\r\n"))
		    req, err := http.NewRequest("GET", urlString, nil)
			if err != nil {
				fmt.Printf("error in creating request: %s\n", err)
			}		
		    req.Header.Set("If-Modified-Since", timeString)
		    req.Header.Set("Connection", "close")
		    req.Header.Set("Accept-Encoding", "gzip")
		    req.Header.Set("Host", host)
		    req.Write(conn)
		    // conn.Write([]byte("\r\n\r\n"))
		}
   	} else {
   		// fmt.Println("is it here?")
   		// fmt.Println("nonconditional?")
   		// fmt.Println(getPath(urlString))

   		// conn.Write([]byte("GET " + getPath(urlString) + " HTTP/1.1\r\n"))
		req, err := http.NewRequest("GET", urlString, nil)
		if err != nil {
			fmt.Printf("error in creating request: %s\n", err)
		}
    	req.Header.Set("Connection", "close")
    	req.Header.Set("Accept-Encoding", "gzip")
    	req.Header.Set("Host", host)
    	req.Write(conn)
    	// conn.Write([]byte("\r\n\r\n"))
    }

    //response time
    currTime := time.Now()
    bufReader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(bufReader, nil)
	if (err != nil) {
		fmt.Printf("error in readResponse: %s\n", err)
		return
	}
    header := resp.Header

    //currently in cache
    if val, ok := cacheMap[urlString]; ok{
    	// fmt.Println("is it in this if clause?")
    	//check header Cache-Control
    	cacheControlKey := http.CanonicalHeaderKey("Cache-Control")
		cacheControlArray := header[cacheControlKey]

		if (stringInArray("no-cache", cacheControlArray)) {
    		// remove node from list, hashtable
    		cacheLock.Lock()
    		cacheList.Remove(val)
    		delete(cacheMap, urlString)	
    		cacheLock.Unlock()
    		statLock.Lock()
    		cacheEvictions++
    		statLock.Unlock()
    	} else if (resp.StatusCode == 200) { // this happens if the thing has been motified
    		if (uint(resp.ContentLength) > maxObjSize) {
    			cacheLock.Lock()
    			cacheList.Remove(val)
    			delete(cacheMap, urlString)
    			cacheLock.Unlock()
    			statLock.Lock()
    			cacheEvictions++
    			statLock.Unlock()
    		} else {
    			//modify node in list using hashtable
    			cacheLock.Lock()
    			cacheMap[urlString].Value.(*node).response = resp
    			//modify time
    			cacheMap[urlString].Value.(*node).cacheTime = currTime
    			cacheLock.Unlock()
    			statLock.Lock()
    			if (int(resp.ContentLength) != -1) {
    				downloadVolume += int(resp.ContentLength)
    			}
    			statLock.Unlock()
    			//move to front of list
    			cacheLock.Lock()
    			cacheList.MoveToFront(cacheMap[urlString])
    			cacheLock.Unlock()
    		}
    	} else if (resp.StatusCode == 304) {
    		cacheLock.Lock()
    		// modify time
    		cacheMap[urlString].Value.(*node).cacheTime = currTime
    		//move to front of list
    		cacheList.MoveToFront(cacheMap[urlString])
    		cacheLock.Unlock()
    		statLock.Lock()
    		cacheHits++
    		statLock.Unlock()
    		<-channels
    		return
    	}
	} else {
		cacheControlKey := http.CanonicalHeaderKey("Cache-Control")
		cacheControlArray := header[cacheControlKey]
		// fmt.Printf("status code: %d length: %d\n", resp.StatusCode, uint(resp.ContentLength))
		// fmt.Println(!stringInArray("no-cache", cacheControlArray))
		// fmt.Println(uint(resp.ContentLength) < maxObjSize)
		// fmt.Println(resp.StatusCode)
		// fmt.Println(resp.StatusCode == 200)
    	if (!stringInArray("no-cache", cacheControlArray) && uint(resp.ContentLength) < maxObjSize && resp.StatusCode == 200) {
    		//insert into hashtable, list
			newNode := node{url: urlString, response: resp, cacheTime: currTime}
			fmt.Println("inserting")
			cacheLock.Lock()
			newElement := cacheList.PushFront(&newNode)
			cacheMap[urlString] = newElement
			cacheLock.Unlock()
			statLock.Lock()
			if (int(resp.ContentLength) != -1) {
				downloadVolume += int(resp.ContentLength)
			}
			statLock.Unlock()
			// update the size of the cache
			cacheLock.Lock()
			cacheSize = cacheSize + uint(resp.ContentLength)
			cacheLock.Unlock()
    	}
    }
   
    //evict using LRU
    for (cacheSize > maxCacheSize) {
    	// remove from list
    	// remove from hashtable
    	cacheLock.Lock()
    	removal := cacheList.Back().Value.(*node)
    	removalUrl := removal.url
    	cacheList.Remove(cacheList.Back())
    	delete(cacheMap, removalUrl)
    	cacheSize = cacheSize - uint(removal.response.ContentLength)
    	cacheLock.Unlock()
    	statLock.Lock()
    	cacheEvictions++
    	statLock.Unlock()
    }
    statLock.Lock()
    cacheMisses++
    statLock.Unlock()
    <-channels
}

func doLinkPrefetching (doc *html.Node, host string) {
	urls := parse(doc)
	fmt.Println(len(urls))
	channels := make(chan bool, maxConcurrency)
	for _, url := range urls {
	   channels <- true
	   // fmt.Printf("prefetched %s\n", url)
	   go handleUrl(url, host, channels)
	}
}

func doDnsPrefetching (doc *html.Node, host string) {
	urls := parse(doc)
	// fmt.Println(urls)

	for _, url := range urls {
		fmt.Println(url)
		if (strings.Contains(url, "http://")) {
			hostStr := getHost(url)
			_, err := net.LookupHost(hostStr)
			if err != nil {
				fmt.Println("ERRRORRRR IN DNSSS PREFETCHINGGG")
			}
		} else {
			// hostStr := getHost(url)
			_, err := net.LookupHost(host)
			if err != nil {
				fmt.Println("ERRRORRRR IN DNSSS PREFETCHINGGG")
			}
		}
	}
}

func handleRequest(w net.Conn) {
	defer w.Close()
	// fmt.Println("entering handleRequest\n")

	reader := bufio.NewReader(w)
	req, err := http.ReadRequest(reader)
	if err != nil {
     	fmt.Println("Error in request")
		fmt.Fprintf(w, Server_Message) // TODO: we should define Server_Message later
		return
    }
    statLock.Lock()
    clientRequests++
    statLock.Unlock()

    method := req.Method
    url := req.URL
    urlString := url.String()
    // fmt.Printf("urlString is: %s\n", urlString)
    if (!strings.HasSuffix(urlString, "/")) {
    	urlString += "/"
    }
    header := req.Header
    var portNum string
    // copied from previous proxy
    host := req.Host
    if !strings.Contains(host, ":") {
       portNum = ":80"
    }
    if strings.Contains(host, ":") {
       portNum = ""
    }
    if method != "GET" {
       fmt.Println("not GET")
       fmt.Fprintf(w, Server_Message) 
       return
    }
    if (strings.Contains(urlString, "https://") || strings.Contains(urlString, "ftp://")) {
    	fmt.Fprintf(w, Server_Message)
    	return
    }
    // fmt.Println("After all checking\n")
    if (caching) {
    	// fmt.Println("caching is on\n")
    	if val, ok := cacheMap[urlString]; ok {
    		currentTime := time.Now()
    		diff := currentTime.Sub(val.Value.(*node).cacheTime)
    		if (uint(diff.Seconds()) < cacheTimeout) {
    			//move node to front of list
    			// TODO: double check with cody about this
    			cacheLock.Lock()
    			cacheList.MoveToFront(val)
    			cacheLock.Unlock()
	    		//send what is in the body
	    		cacheResponse := val.Value.(*node).response
	    		defer cacheResponse.Body.Close()
	    		// fmt.Println("writing to output\n")
				
				var newReader io.Reader
				cacheLock.Lock()

				bodyBytes, err := ioutil.ReadAll(cacheResponse.Body)
				if err != nil {
	    			fmt.Printf("readAll1 error: %s\n", err)
	    		}
				cacheMap[urlString].Value.(*node).response.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
				cacheLock.Unlock()
				tempReader := bytes.NewReader(bodyBytes)
				cacheResponse.Write(w)


				fmt.Println("returning from cache")
				fmt.Println(len(bodyBytes))
				statLock.Lock()
				cacheHits++
				if (int(cacheResponse.ContentLength) != -1) {
					trafficSent += int(cacheResponse.ContentLength)
					volumeFromCache += int(cacheResponse.ContentLength)
				}
				statLock.Unlock()
				cacheLock.Lock()
				cacheMap[urlString].Value.(*node).response.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
				cacheLock.Unlock()
				fmt.Println(len(bodyBytes))

				encodingKey := http.CanonicalHeaderKey("Content-Encoding")
				encodingArray := cacheResponse.Header[encodingKey]

				if (stringInArray("gzip", encodingArray)) {
					newReader, err = gzip.NewReader(tempReader)
			    	if err != nil {
						fmt.Println(err) 
						return
					}
				} else {
					newReader = tempReader
				}

				doc, err := html.Parse(newReader)
				if err != nil { 
					return
				}
				if linkPrefetching {
					go doLinkPrefetching(doc, host)
				} else if dnsPrefetching {
					go doDnsPrefetching(doc, host) 
				}
	    		return
    		} else {
    			//conditional GET
	    		//add to header If-Modified-Since
	    		fmt.Println("calling conditional GET")
	    		modifiedTime := val.Value.(*node).cacheTime.UTC()
	    		timeString := modifiedTime.Format("Mon, 02 Jan 2006 15:04:05 GMT")
	    		header.Set("If-Modified-Since", timeString)
    		}
    	}
    }

    connDial, err := net.Dial("tcp", host + portNum)
    if err != nil {
       fmt.Println("Error in Dial")
       fmt.Fprintf(w, Server_Message)
       return
    }
    defer connDial.Close()
    path := url.Path
    connDial.Write([]byte("GET " + path + " HTTP/1.1\r\n"))
    // req, err := http.NewRequest("GET", path, nil)
    header.Set("Host", host)
    header.Set("Connection", "close")
    header.Set("Accept-Encoding", "gzip")
    header.Write(connDial)
    connDial.Write([]byte("\r\n\r\n"))
    // fmt.Println("after dial\n")
    
    bufReader := bufio.NewReader(connDial)
    // fmt.Println("before response\n")
    // fmt.Println(req);
    resp, err := http.ReadResponse(bufReader, req)
    // fmt.Println("after response\n")
    if err != nil {
     	fmt.Println("Error in response")
		fmt.Fprintf(w, Server_Message) // TODO: define Server_Message
		return
    }
    defer resp.Body.Close()

    //response time
    currTime := time.Now()
    header = resp.Header
    contentLength := resp.ContentLength

    if (caching) {
	    //currently in cache
	    if val, ok := cacheMap[urlString]; ok{
	    	//check header Cache-Control
	    	cacheControlKey := http.CanonicalHeaderKey("Cache-Control")
			cacheControlArray := header[cacheControlKey]

			// remove node from list, hashtable
			if (stringInArray("no-cache", cacheControlArray)) {
	    		cacheLock.Lock()
	    		cacheList.Remove(val)
	    		delete(cacheMap, urlString)
	    		cacheLock.Unlock()
	    		statLock.Lock()
	    		cacheEvictions++
	    		statLock.Unlock()
	    	} else if (resp.StatusCode == 200) {   				//the cached object has been modified
	    		if (uint(resp.ContentLength) > maxObjSize) {
	    			cacheLock.Lock()
	    			cacheList.Remove(val)
	    			delete(cacheMap, urlString)
	    			cacheLock.Unlock()
	    			statLock.Lock()
	    			cacheEvictions++
	    			statLock.Unlock()
	    		} else {
	    			//modify node in list using hashtable
	    			cacheLock.Lock()
	    			cacheMap[urlString].Value.(*node).response = resp
	    			//modify time
	    			cacheMap[urlString].Value.(*node).cacheTime = currTime
	    			cacheLock.Unlock()
	    			statLock.Lock()
	    			if (int(resp.ContentLength) != -1) {
	    				downloadVolume += int(resp.ContentLength)
	    			}
	    			statLock.Unlock()
	    			//move to front of list
	    			cacheLock.Lock()
	    			cacheList.MoveToFront(val)
	    			cacheLock.Unlock()
	    		}
	    	} else if (resp.StatusCode == 304) {   //the cached object has not been modified
	    		// modify time
	    		cacheLock.Lock()
	    		cacheMap[urlString].Value.(*node).cacheTime = currTime
	    		//move to front of list
	    		cacheList.MoveToFront(val)
	    		cacheLock.Unlock()
	    		cacheResponse := val.Value.(*node).response
	    		if (cacheResponse.ContentLength == -1) {
	    			fmt.Println("fuc")
	    		}
	    		defer cacheResponse.Body.Close()

	    		var newReader io.Reader
	    		cacheLock.Lock()
	    		bodyBytes, err := ioutil.ReadAll(cacheResponse.Body)
	    		if err != nil {
	    			fmt.Printf("readAll2 error: %s\n", err)
	    		}
				cacheMap[urlString].Value.(*node).response.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
				cacheLock.Unlock()
				tempReader := bytes.NewReader(bodyBytes)


				fmt.Println("it's in the cache and has not been modified")
	    		cacheResponse.Write(w)

	    		statLock.Lock()
	    		cacheHits++
	    		if (int(cacheResponse.ContentLength) != -1) {
		    		trafficSent += int(cacheResponse.ContentLength)
		    		volumeFromCache += int(cacheResponse.ContentLength)
		    	}
	    		statLock.Unlock()
	    		cacheLock.Lock()
	    		cacheMap[urlString].Value.(*node).response.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
	    		cacheLock.Unlock()

	    		encodingKey := http.CanonicalHeaderKey("Content-Encoding")
				encodingArray := cacheResponse.Header[encodingKey]

				if (stringInArray("gzip", encodingArray)) {
					newReader, err = gzip.NewReader(tempReader)
			    	if err != nil {
						fmt.Println(err) 
						return
					}	
				} else {
					newReader = tempReader
				}

				doc, err := html.Parse(newReader)
				if err != nil { 
					return
				}

				if linkPrefetching {
					go doLinkPrefetching(doc, host)
				} else if dnsPrefetching {
					go doDnsPrefetching(doc, host) 
				}
	    		return
	    	}
	    	
    	} else {
    		cacheControlKey := http.CanonicalHeaderKey("Cache-Control")
			cacheControlArray := header[cacheControlKey]

	    	if (!stringInArray("no-cache", cacheControlArray) && uint(contentLength) < maxObjSize && resp.StatusCode == 200) {
	    		//insert into hashtable, list
				fmt.Println("caching in")
				newNode := node{url: urlString, response: resp, cacheTime: currTime}
				cacheLock.Lock()
				newElement := cacheList.PushFront(&newNode)
				cacheMap[urlString] = newElement
				cacheLock.Unlock()
				// update the size of the cache
				statLock.Lock()
				if (int(resp.ContentLength) != -1) {
					downloadVolume += int(resp.ContentLength)
				}
				statLock.Unlock()
				cacheLock.Lock()
				cacheSize = cacheSize + uint(contentLength)
				cacheLock.Unlock()
	    	}
	    }
	    //evict using LRU
	    for (cacheSize > maxCacheSize) {
	    	// remove from list
	    	// remove from hashtable
	    	cacheLock.Lock()
	    	removal := cacheList.Back().Value.(*node)
	    	removalUrl := removal.url
	    	cacheList.Remove(cacheList.Back())
	    	delete(cacheMap, removalUrl)
	    	cacheSize = cacheSize - uint(removal.response.ContentLength)
	    	cacheLock.Unlock()
	    	statLock.Lock()
	    	cacheEvictions++
	    	statLock.Unlock()
	    }
    }
    cacheLock.Lock()
    if (resp.ContentLength == -1) {
	    			fmt.Println("fuc")
	    		}
    bodyBytes, err := ioutil.ReadAll(resp.Body)
    if err != nil {
	    fmt.Printf("readAll3 error: %s\n", err)
	}
	resp.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
	cacheLock.Unlock()

	resp.Write(w)
	fmt.Println("writing to client")
	statLock.Lock()
	cacheMisses++
	if (resp.ContentLength != -1) {
		trafficSent += int(resp.ContentLength)
	}
	statLock.Unlock()
	cacheLock.Lock()
	if (caching) {
		if _, ok := cacheMap[urlString]; ok {
			cacheMap[urlString].Value.(*node).response.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
		}
	}
	cacheLock.Unlock()
	if resp.StatusCode == 200 {
		// fmt.Println("dns/link prefetching\n")

		var newReader io.Reader
		// fmt.Println("writing to output finallll\n")
		tempReader := bytes.NewReader(bodyBytes)

		encodingKey := http.CanonicalHeaderKey("Content-Encoding")
		encodingArray := header[encodingKey]

		if (stringInArray("gzip", encodingArray)) {
			newReader, err = gzip.NewReader(tempReader)
			if err != nil {
				fmt.Println(err) 
				return
			}	
		} else {
			newReader = tempReader
		}

		doc, err := html.Parse(newReader)
		if err != nil { 
			fmt.Println(err)
			return
		}

		if linkPrefetching {
			go doLinkPrefetching(doc, host)
		} else if dnsPrefetching {
			go doDnsPrefetching(doc, host) 
		}
	}
}

func initFlags() {
	flag.UintVar(&listeningPort, "port", 8080, "Proxy listening port")
	flag.BoolVar(&dnsPrefetching, "dns", false, "Enable DNS prefetching")
	flag.BoolVar(&caching, "cache", false, "Enable object caching")
	flag.UintVar(&cacheTimeout, "timeout", 120, "Cache timeout in seconds")
	flag.UintVar(&maxCacheSize, "max_cache", MAX_CACHE_SIZE, "Maximum cache size")
	flag.UintVar(&maxObjSize, "max_obj", MAX_OBJ_SIZE, "Maximum object size")
	flag.BoolVar(&linkPrefetching, "link", false, "Enable link prefetching")
	flag.UintVar(&maxConcurrency, "max_conc", 10, "Number of threads for link prefetching")
	flag.StringVar(&outputFile, "file", "proxy.log", "Output file name")
	flag.Parse()
}

func main() {
	initFlags()

	go saveStatistics()
	// TODO: Other initializations
	ln, er := net.Listen("tcp", "0.0.0.0:" + strconv.Itoa(int(listeningPort)))
	if er != nil {
       fmt.Println("Error in Listen")
       return
    }

    defer ln.Close()

    if (linkPrefetching) {
    	caching = true
	}
    // I should initialize my cache structures here	
    if (caching) {
    	// doubly-linked list and Map for the LRU cache data-structure
    	cacheMap = make(map[string]*list.Element)
    	cacheList = list.New()
    	cacheSize = 0
    }
    fmt.Println("hello")
    statLock = &sync.RWMutex{}
    cacheLock = &sync.Mutex{}

	for {
		// fmt.Println("hi")
		conn, error := ln.Accept()
	   	if error != nil {
	       fmt.Println("Error in Accept")
  	    }
        go handleRequest(conn)
        // debugCache()
    }
}
