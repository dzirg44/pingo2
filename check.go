package main

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// minimum interval between checks. Used as default value when none set by user.
const CheckInterval = 30

// don't alert if host goes down and comes back within this time span
const StandoffInterval = 60

type Target struct {
	// target id
	Id int
	// Name of the Target
	Name string
	// Address of the target e.g. "http://localhost"
	Addr string
	// HTTP 'Host:' header (if different from Addr)
	Host string
	// Polling interval, in seconds
	Interval int
	// Look for this string in the response body
	Keyword string
	// Run specific  command
	Commandrun string
}

type TargetStatus struct {
	Target    *Target
	Online    bool
	ErrorMsg  string
	Since     time.Time
	LastCheck time.Time
	LastAlert time.Time
}

func startTarget(t Target, res chan TargetStatus, config Config) {
	go runTarget(t, res, config)
}

func runTarget(t Target, res chan TargetStatus, config Config) {
	var err error
	var failed bool
	var addrURL *url.URL
	log.Printf("starting runtarget on %s", t.Name)
	if t.Interval < CheckInterval {
		t.Interval = CheckInterval
	}

	addrURL, err = url.Parse(t.Addr)
	if err != nil {
		log.Printf("[%d:-] target address %s could not be read, %s", t.Id, addrURL, err)
		return
	}
	fmt.Println(t.Commandrun)
	if config.Standoff == 0 {
		config.Standoff = StandoffInterval
	} else if config.Standoff <= t.Interval {
		log.Printf("[%d:%s] Standoff %d can't be <= Interval %d, Standoff now %d\n", t.Id, addrURL, config.Standoff, t.Interval, t.Interval+1)
		config.Standoff = t.Interval + 1
	}

	// wait a bit, to randomize check offset
	time.Sleep(time.Duration(rand.Intn(t.Interval)) * time.Second)

	ticker := time.Tick(time.Duration(t.Interval) * time.Second)
	alertRequest := make(chan *TargetStatus, 1)
	// spawn routine to handle alert requests
	go alertRoutine(alertRequest, config)
	status := TargetStatus{Target: &t, Online: true, Since: time.Now()}

	for {
		failed = false
		status.ErrorMsg = ""

		// Polling
		switch addrURL.Scheme {
		case "http", "https":
			var resp *http.Response
			var client *http.Client

			req, _ := http.NewRequest("GET", addrURL.String(), nil)
			transport := &http.Transport{
				DisableKeepAlives:  true,
				DisableCompression: true,
			}
			if t.Host != "" {
				// Set hostname for TLS connection. This allows us to connect using
				// another hostname or IP for the actual TCP connection. Handy for GeoDNS scenarios.
				transport.TLSClientConfig = &tls.Config{
					ServerName: t.Host,
				}
				req.Host = t.Host
			}
			client = &http.Client{
				Timeout:   time.Duration(config.Timeout) * time.Second,
				Transport: transport,
			}
			resp, err = client.Do(req)
			if err != nil {
				log.Printf("[%d:%s] http(s) error, %s", t.Id, addrURL, err)
				status.ErrorMsg = fmt.Sprintf("%s", err)
				failed = true
			} else {
				var body []byte
				body, err = ioutil.ReadAll(resp.Body)
				if err != nil {
					log.Printf("[%d:%s] http(s) error, %s", t.Id, addrURL, err)
					status.ErrorMsg = fmt.Sprintf("%s", err)
					failed = true
				} else {
					if t.Keyword != "" {
						if strings.Index(string(body), t.Keyword) == -1 {
							status.ErrorMsg = fmt.Sprintf("keyword '%s' not found", t.Keyword)
							log.Printf("[%d:%s] http(s) error, %s", t.Id, addrURL, status.ErrorMsg)
							failed = true
						}
					}
				}
				resp.Body.Close()
			}
		case "ping":
			var success bool
			success, err = Ping(addrURL.Host)
			if err != nil {
				log.Printf("[%d:%s] ping error, %s", t.Id, addrURL, err)
				status.ErrorMsg = fmt.Sprintf("%s", err)
			}
			failed = !success
		default:
			var conn net.Conn
			conn, err = net.DialTimeout("tcp", addrURL.Host, time.Duration(config.Timeout)*time.Second)
			if err != nil {
				log.Printf("[%d:%s] tcp conn error, %s", t.Id, addrURL, err)
				status.ErrorMsg = fmt.Sprintf("%s", err)
				failed = true
			} else {
				conn.Close()
			}
		}

		status.LastCheck = time.Now()

		if debug {
			log.Printf("[%d:%s] failed=%v, online=%v, since=%s, last_alert=%s, last_check=%s", t.Id, addrURL, failed, status.Online, status.Since, status.LastAlert, status.LastCheck)
		}

		if failed {
			// Error during connect
			if status.Online {
				// was online, now offline
				status.Online = false
				status.Since = time.Now()
				alertRequest <- &status

			} else {
				// was offline, still offline
				if time.Since(status.LastAlert) > time.Second*time.Duration(config.Alert.Interval) {
					alertRequest <- &status
				}
			}
		} else {
			// Connect ok
			if !status.Online {
				// was offline, now online
				status.Online = true
				if debug {
					log.Printf("[%d:%s] was offline, now online - time since=%s", t.Id, addrURL, time.Since(status.Since))
				}
				alertRequest <- &status
			}
		}

		res <- status

		// waiting for ticker
		<-ticker
	}
}

func alert(status *TargetStatus, config Config) {
	if status.Target.Commandrun != "" {
		command := status.Target.Commandrun
		err := Commandrun(command, config)
		if err != nil {
			log.Printf("%s", err)
		}
		log.Printf("[%d:%s] alert sent to %s", status.Target.Id, status.Target.Addr, config.Alert.ToEmail, status.Target.Commandrun)
	} else {
		if debug {
			log.Printf("[%d:%s] alert NOT sent as no 'To:' email specified", status.Target.Id, status.Target.Addr)
		}
	}

	if config.Alert.ToEmail != "" {
		err := EmailAlert(*status, config)
		if err != nil {
			log.Printf("%s", err)
		}
		log.Printf("[%d:%s] alert sent to %s", status.Target.Id, status.Target.Addr, config.Alert.ToEmail)
	} else {
		if debug {
			log.Printf("[%d:%s] alert NOT sent as no 'To:' email specified", status.Target.Id, status.Target.Addr)
		}
	}
	status.LastAlert = time.Now()
}

func alertRoutine(alertRequest <-chan *TargetStatus, config Config) {

	for {
		select {
		case req := <-alertRequest:
			// Host is online, or has been offline for greater than a minute
			if req.Online || time.Since(req.Since) > time.Duration(time.Minute) {
				alert(req, config)
			} else {
				// Don't bother with 'down' alert
				// if host comes back within standoff time
				timer1 := time.NewTimer(time.Duration(config.Standoff) * time.Second)
				for {
					select {
					case req2 := <-alertRequest:
						if req2.Online {
							// Don't bother with 'up' alert if the host was down less than standoff time
							if time.Since(req2.Since) > time.Duration(config.Standoff)*time.Second {
								alert(req2, config)
							} else {
								if debug {
									log.Printf("[%d:%s] down/up alerts skipped due to standoff", req.Target.Id, req.Target.Addr)
								}
							}
							req2.Since = time.Now()
							goto done
						} else {
							// if another 'offline' requests comes in the meantime
							// ignore and continue waiting for timer or 'online'
							continue
						}
					case <-timer1.C:
						alert(req, config)
					}

				done:
					break
				}
			}
		}
	}
}
