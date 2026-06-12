package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config from environment variables
var (
	hlsURL         = getEnv("HLS_URL", "https://stream.studiorhedenlive.nl/hlstv/stream.m3u8")
	alertURL       = getEnv("ALERT_URL", "https://uptime.fardau.com/api/push/mGIWEeKvvT1cFdqjMdesZph3v8hxtHfl?status=up&msg=HLSTVStreamFail&ping=")
	segmentTimeout = getDurationEnv("SEGMENT_TIMEOUT", 15*time.Second)
	failCooldown   = getDurationEnv("FAIL_COOLDOWN", 60*time.Second)
)

var httpClient = &http.Client{
	Timeout: 20 * time.Second,
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getDurationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
		log.Printf("Warning: invalid duration for %s: %s", key, v)
	}
	return fallback
}

func fetchURL(rawURL string, timeout time.Duration) ([]byte, error) {
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d %s", resp.StatusCode, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body failed: %w", err)
	}
	return body, nil
}

// Playlist holds the parsed result of a media playlist.
type Playlist struct {
	Segments      []string      // ordered segment URLs
	TargetDuration time.Duration // #EXT-X-TARGETDURATION value
	LastExtInf    time.Duration // duration of the last segment (#EXTINF)
}

// resolveMediaURL follows a master playlist to its first media playlist URL,
// or returns the original URL unchanged if it is already a media playlist.
func resolveMediaURL(rawURL string) (string, error) {
	body, err := fetchURL(rawURL, segmentTimeout)
	if err != nil {
		return "", fmt.Errorf("playlist fetch failed: %w", err)
	}
	content := string(body)
	if !strings.HasPrefix(strings.TrimSpace(content), "#EXTM3U") {
		return "", fmt.Errorf("response is not a valid M3U8 playlist")
	}
	if !strings.Contains(content, "#EXT-X-STREAM-INF") {
		// Already a media playlist
		return rawURL, nil
	}

	// Master playlist — extract first variant URL
	base, _ := url.Parse(rawURL)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			continue
		}
		return base.ResolveReference(u).String(), nil
	}
	return "", fmt.Errorf("master playlist has no variant streams")
}

// parseMediaPlaylist parses a media playlist body, resolving relative URLs
// against baseURL.
func parseMediaPlaylist(baseURL, body string) (*Playlist, error) {
	pl := &Playlist{}
	base, _ := url.Parse(baseURL)

	var pendingExtInf time.Duration
	scanner := bufio.NewScanner(strings.NewReader(body))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// #EXT-X-TARGETDURATION:<seconds>
		if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			val := strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")
			if secs, err := strconv.ParseFloat(val, 64); err == nil {
				pl.TargetDuration = time.Duration(secs * float64(time.Second))
			}
			continue
		}

		// #EXTINF:<duration>,<title>
		if strings.HasPrefix(line, "#EXTINF:") {
			raw := strings.TrimPrefix(line, "#EXTINF:")
			// Duration is everything before the first comma
			if idx := strings.IndexByte(raw, ','); idx >= 0 {
				raw = raw[:idx]
			}
			if secs, err := strconv.ParseFloat(raw, 64); err == nil {
				pendingExtInf = time.Duration(secs * float64(time.Second))
			}
			continue
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		// Segment URL
		u, err := url.Parse(line)
		if err != nil {
			continue
		}
		pl.Segments = append(pl.Segments, base.ResolveReference(u).String())
		pl.LastExtInf = pendingExtInf
		pendingExtInf = 0
	}

	if len(pl.Segments) == 0 {
		return nil, fmt.Errorf("no segments found in media playlist")
	}
	return pl, nil
}

// pollInterval returns the correct delay before the next playlist refresh,
// following RFC 8216 §6.3.4:
//   - If the playlist has not changed (same last segment), wait half the
//     target duration before retrying.
//   - After a successful refresh with a new segment, wait for the duration
//     of the last segment (EXTINF) before the next fetch.
//   - Fall back to half the target duration if EXTINF is missing.
func nextPollDelay(pl *Playlist, newSegment bool) time.Duration {
	target := pl.TargetDuration
	if target == 0 {
		target = 8 * time.Second // conservative default if tag is absent
	}
	if !newSegment {
		// Playlist unchanged — retry at half target duration
		return target / 2
	}
	// New segment available — sleep for its reported duration
	if pl.LastExtInf > 0 {
		return pl.LastExtInf
	}
	return target / 2
}

func fireAlert(reason string) {
	// Parse the base alert URL and set the ping parameter safely,
	// avoiding broken URLs when the reason contains colons or slashes.
	u, err := url.Parse(alertURL)
	if err != nil {
		log.Printf("Alert URL is invalid: %v", err)
		return
	}
	q := u.Query()
	q.Set("ping", reason)
	u.RawQuery = q.Encode()
	fullURL := u.String()

	log.Printf("\U0001f6a8 Firing alert | reason: %s", reason)
	resp, err := httpClient.Get(fullURL)
	if err != nil {
		log.Printf("Alert request failed: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("Alert response: %d", resp.StatusCode)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[hls-monitor] ")

	log.Printf("Starting HLS monitor")
	log.Printf("  Stream:          %s", hlsURL)
	log.Printf("  Alert URL:       %s", alertURL)
	log.Printf("  Segment timeout: %s", segmentTimeout)
	log.Printf("  Fail cooldown:   %s", failCooldown)

	// Resolve master → media playlist URL once at startup.
	// If the stream is unavailable at boot we still proceed; resolution is
	// retried on each failure cycle.
	mediaURL := hlsURL

	var (
		lastAlert      time.Time
		lastSegmentURL string
	)

	for {
		iterStart := time.Now()

		// --- 1. Resolve media playlist URL (handles master playlists) ---
		resolved, err := resolveMediaURL(mediaURL)
		if err != nil {
			log.Printf("❌ Could not resolve media playlist: %v", err)
			maybeAlert(err.Error(), &lastAlert)
			time.Sleep(8 * time.Second)
			continue
		}
		mediaURL = resolved

		// --- 2. Fetch & parse the media playlist ---
		body, err := fetchURL(mediaURL, segmentTimeout)
		if err != nil {
			log.Printf("❌ Media playlist fetch failed: %v", err)
			maybeAlert(err.Error(), &lastAlert)
			time.Sleep(8 * time.Second)
			continue
		}

		pl, err := parseMediaPlaylist(mediaURL, string(body))
		if err != nil {
			log.Printf("❌ Media playlist parse failed: %v", err)
			maybeAlert(err.Error(), &lastAlert)
			time.Sleep(8 * time.Second)
			continue
		}

		// --- 3. Check whether the playlist has a new segment ---
		latestSeg := pl.Segments[len(pl.Segments)-1]
		newSegment := latestSeg != lastSegmentURL

		if newSegment {
			// --- 4. Fetch the new segment to confirm it is readable ---
			log.Printf("New segment (%d in playlist, target=%s, extinf=%s): %s",
				len(pl.Segments), pl.TargetDuration, pl.LastExtInf, latestSeg)

			segBody, err := fetchURL(latestSeg, segmentTimeout)
			if err != nil {
				log.Printf("❌ Segment fetch failed: %v", err)
				maybeAlert(fmt.Sprintf("segment fetch failed: %v", err), &lastAlert)
				time.Sleep(nextPollDelay(pl, false))
				continue
			}
			if len(segBody) == 0 {
				msg := fmt.Sprintf("segment was empty: %s", latestSeg)
				log.Printf("❌ %s", msg)
				maybeAlert(msg, &lastAlert)
				time.Sleep(nextPollDelay(pl, false))
				continue
			}

			log.Printf("✅ Stream OK — segment %d bytes, fetched in %s",
				len(segBody), time.Since(iterStart).Round(time.Millisecond))
			lastSegmentURL = latestSeg
		} else {
			log.Printf("⏳ Playlist unchanged, last segment: %s", latestSeg)
		}

		delay := nextPollDelay(pl, newSegment)
		log.Printf("   Next poll in %s", delay)
		time.Sleep(delay)
	}
}

// maybeAlert fires an alert if the cooldown has elapsed.
func maybeAlert(reason string, lastAlert *time.Time) {
	if time.Since(*lastAlert) > failCooldown {
		fireAlert(reason)
		*lastAlert = time.Now()
	} else {
		log.Printf("   Alert suppressed (cooldown, next in %s)",
			(failCooldown - time.Since(*lastAlert)).Round(time.Second))
	}
}
