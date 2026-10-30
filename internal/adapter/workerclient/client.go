package workerclient

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const Preset = "avif-q50-s6-420-v1"

type Job struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	Preset       string `json:"preset"`
	SourceSHA1   string `json:"sourceSha1"`
	SourceSHA256 string `json:"sourceSha256"`
	SourceSize   int64  `json:"sourceSize"`
	LeaseToken   string `json:"leaseToken"`
	Size         int64  `json:"size"`
	Lease        struct {
		Token string `json:"token"`
	} `json:"lease"`
}
type Config struct {
	ServerURL, Token, WorkerID, TempDir, Converter string
	PollInterval, HeartbeatInterval                time.Duration
	HTTPClient                                     *http.Client
	Logger                                         *log.Logger
}
type Client struct {
	cfg     Config
	base    *url.URL
	convert func(context.Context, string, string, string) (string, error)
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.ServerURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("server URL must be an http(s) URL without query or fragment")
	}
	if cfg.Token == "" || cfg.WorkerID == "" {
		return nil, errors.New("worker token and ID are required")
	}
	if cfg.Converter == "" {
		cfg.Converter = "jpeg-to-avif.sh"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 20 * time.Second
	}
	if cfg.HTTPClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		transport.TLSHandshakeTimeout = 10 * time.Second
		transport.ResponseHeaderTimeout = 30 * time.Second
		cfg.HTTPClient = &http.Client{Transport: transport}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Client{cfg: cfg, base: u, convert: runConverter}, nil
}
func (c *Client) request(ctx context.Context, method, path string, body io.Reader, lease string) (*http.Response, error) {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("X-Worker-ID", c.cfg.WorkerID)
	if lease != "" {
		req.Header.Set("X-Lease-Token", lease)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.cfg.HTTPClient.Do(req)
}
func status(resp *http.Response) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
}
func (c *Client) claim(ctx context.Context) (*Job, error) {
	b, _ := json.Marshal(map[string]string{"workerId": c.cfg.WorkerID})
	resp, err := c.request(ctx, "POST", "/internal/jobs/claim", bytes.NewReader(b), "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		resp.Body.Close()
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, status(resp)
	}
	defer resp.Body.Close()
	var job Job
	if err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&job); err != nil {
		return nil, fmt.Errorf("decode claim: %w", err)
	}
	if job.LeaseToken == "" {
		job.LeaseToken = job.Lease.Token
	}
	if job.SourceSize == 0 {
		job.SourceSize = job.Size
	}
	if job.ID == "" || job.LeaseToken == "" || strings.ContainsAny(job.ID, "/\\") || job.ID == "." || job.ID == ".." {
		return nil, errors.New("invalid job ID or lease token")
	}
	return &job, nil
}
func jobPath(job *Job, action string) string {
	return "/internal/jobs/" + job.ID + "/" + action
}
func (c *Client) heartbeat(ctx context.Context, j *Job) error {
	resp, err := c.request(ctx, "POST", jobPath(j, "heartbeat"), nil, j.LeaseToken)
	if err != nil {
		return err
	}
	return status(resp)
}

type failure struct {
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
	Skipped   bool   `json:"skipped"`
	WorkerID  string `json:"workerId"`
	Token     string `json:"token"`
}

func (c *Client) fail(ctx context.Context, j *Job, f failure) error {
	f.WorkerID, f.Token = c.cfg.WorkerID, j.LeaseToken
	b, _ := json.Marshal(f)
	resp, err := c.request(ctx, "POST", jobPath(j, "fail"), bytes.NewReader(b), j.LeaseToken)
	if err != nil {
		return err
	}
	return status(resp)
}
func (c *Client) download(ctx context.Context, j *Job, path string) error {
	if j.SourceSize <= 0 || j.SourceSize == int64(^uint64(0)>>1) || len(j.SourceSHA1) != 40 || len(j.SourceSHA256) != 64 {
		return errors.New("invalid source size or digests")
	}
	resp, err := c.request(ctx, "GET", jobPath(j, "source"), nil, j.LeaseToken)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return status(resp)
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	h1, h2 := sha1.New(), sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h1, h2), io.LimitReader(resp.Body, j.SourceSize+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != j.SourceSize || !strings.EqualFold(hex.EncodeToString(h1.Sum(nil)), j.SourceSHA1) || !strings.EqualFold(hex.EncodeToString(h2.Sum(nil)), j.SourceSHA256) {
		return errors.New("source size or digest mismatch")
	}
	return nil
}
func (c *Client) upload(ctx context.Context, j *Job, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h1, h2 := sha1.New(), sha256.New()
	if _, err = io.Copy(io.MultiWriter(h1, h2), f); err != nil {
		return err
	}
	if _, err = f.Seek(0, 0); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + jobPath(j, "result")
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, "PUT", u.String(), f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("X-Worker-ID", c.cfg.WorkerID)
	req.Header.Set("X-Lease-Token", j.LeaseToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	name := strings.TrimSuffix(filepath.Base(strings.ReplaceAll(j.Filename, "\\", "/")), filepath.Ext(j.Filename)) + ".avif"
	if strings.ContainsAny(name, "\r\n") {
		return errors.New("invalid output filename")
	}
	req.Header.Set("X-Output-Filename", name)
	req.Header.Set("X-Output-SHA1", hex.EncodeToString(h1.Sum(nil)))
	req.Header.Set("X-Output-SHA256", hex.EncodeToString(h2.Sum(nil)))
	req.Header.Set("X-Output-Size", strconv.FormatInt(info.Size(), 10))
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	return status(resp)
}
func (c *Client) process(ctx context.Context, j *Job) failure {
	if j.Preset != Preset {
		return failure{Error: "unsupported preset", Skipped: true}
	}
	ext := strings.ToLower(filepath.Ext(j.Filename))
	if ext != ".jpg" && ext != ".jpeg" {
		return failure{Error: "unsupported file type", Skipped: true}
	}
	dir, err := os.MkdirTemp(c.cfg.TempDir, "optimizer-worker-")
	if err != nil {
		return failure{Error: err.Error(), Retryable: true}
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "source.jpg")
	out := filepath.Join(dir, "output.avif")
	if err = c.download(ctx, j, source); err != nil {
		return failure{Error: "download: " + err.Error(), Retryable: true}
	}
	message, err := c.convert(ctx, c.cfg.Converter, source, out)
	if err != nil {
		skipped := strings.Contains(message, "unsupported multi-picture JPEG") || strings.Contains(message, "unsupported motion or HDR JPEG") || strings.Contains(message, "input is not a JPEG") || strings.Contains(message, "unsupported EXIF orientation")
		return failure{Error: fmt.Sprintf("converter: %v: %s", err, message), Retryable: !skipped, Skipped: skipped}
	}
	info, err := os.Stat(out)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return failure{Error: "converter produced no valid output", Retryable: true}
	}
	if info.Size() >= j.SourceSize {
		return failure{Error: "output is not smaller than source", Skipped: true}
	}
	if err = c.upload(ctx, j, out); err != nil {
		return failure{Error: "upload: " + err.Error(), Retryable: true}
	}
	return failure{}
}
func (c *Client) RunOnce(ctx context.Context) (bool, error) {
	j, err := c.claim(ctx)
	if err != nil || j == nil {
		return false, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(c.cfg.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-t.C:
				hctx, hcancel := context.WithTimeout(jobCtx, 10*time.Second)
				err := c.heartbeat(hctx, j)
				hcancel()
				if err != nil {
					c.cfg.Logger.Printf("heartbeat job %s: %v", j.ID, err)
				}
			}
		}
	}()
	result := c.process(jobCtx, j)
	cancel()
	<-done
	if result.Error != "" && ctx.Err() == nil {
		fctx, fcancel := context.WithTimeout(ctx, 15*time.Second)
		defer fcancel()
		if err := c.fail(fctx, j, result); err != nil {
			return true, fmt.Errorf("report failure (%s): %w", result.Error, err)
		}
		c.cfg.Logger.Printf("job %s: %s", j.ID, result.Error)
	}
	return true, ctx.Err()
}
func (c *Client) Run(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := c.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			c.cfg.Logger.Printf("worker: %v", err)
		}
		if !worked || err != nil {
			timer := time.NewTimer(c.cfg.PollInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
	}
}
