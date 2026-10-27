package guestbin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/tarx"
)

// OpFetch and OpPost take a tar from a server or post one to it over
// HTTP, on a credential the call's header alone carries, after a probe
// that ends the call with ExitUnreachable, nothing sent, if it fails.

// HTTPAnswer is how the server answered: its status, its result header
// (internal/system's ResultHeader), and its message when it failed.
type HTTPAnswer struct {
	Status  int    `json:"status"`
	Result  string `json:"result,omitempty"`
	Message string `json:"message,omitempty"`
}

// HTTPResultHeader is the header an answer's Result is read from.
const HTTPResultHeader = "Tower-Result"

// probeTimeout bounds the probe, its connection included.
const probeTimeout = 15 * time.Second

// httpClient connects within probeTimeout; what follows lasts as long as
// the call does.
func httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: probeTimeout}).DialContext,
		ResponseHeaderTimeout: 10 * time.Minute,
	}}
}

// callContext ends as the call's stdin does, which says the call is gone.
func callContext(in io.Reader) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = io.Copy(io.Discard, in)
		cancel()
	}()
	return ctx, cancel
}

// probe asks the probe's URL, which must answer 200.
func probe(ctx context.Context, c *http.Client, url string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("the probe answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// answerOf reads a response that is no success into its answer.
func answerOf(resp *http.Response) HTTPAnswer {
	a := HTTPAnswer{Status: resp.StatusCode, Result: resp.Header.Get(HTTPResultHeader)}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		a.Message = strings.TrimSpace(string(msg))
	}
	return a
}

func writeAnswer(stdout io.Writer, a HTTPAnswer) int {
	if err := json.NewEncoder(stdout).Encode(a); err != nil {
		return ExitFailed
	}
	return 0
}

func checkHTTP(h Header) error {
	if h.URL == "" || h.Probe == "" || h.Token == "" || !filepath.IsAbs(h.Path) {
		return errors.New("an HTTP transfer needs a URL, a probe, a credential and an absolute path")
	}
	return nil
}

// fetchOp GETs URL with the credential and extracts the tar it answers
// into the directory Path, as CopyIn does.
func fetchOp(h Header, in io.Reader, stdout, stderr io.Writer) int {
	if err := checkHTTP(h); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	ctx, cancel := callContext(in)
	defer cancel()
	c := httpClient()
	defer c.CloseIdleConnections()
	if err := probe(ctx, c, h.Probe); err != nil {
		return failed(stderr, ExitUnreachable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	resp, err := c.Do(req)
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	a := answerOf(resp)
	if resp.StatusCode != http.StatusOK {
		return writeAnswer(stdout, a)
	}
	if err := tarx.Extract(resp.Body, h.Path); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	// What follows the tar's end is read, so a body cut short of its
	// length fails.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	return writeAnswer(stdout, a)
}

// postOp POSTs a tar of Path, as CopyOut writes it, to URL with the
// credential.
func postOp(h Header, in io.Reader, stdout, stderr io.Writer) int {
	if err := checkHTTP(h); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	if _, err := os.Lstat(h.Path); errors.Is(err, fs.ErrNotExist) {
		return failed(stderr, ExitNotExist, err)
	}
	ctx, cancel := callContext(in)
	defer cancel()
	c := httpClient()
	defer c.CloseIdleConnections()
	if err := probe(ctx, c, h.Probe); err != nil {
		return failed(stderr, ExitUnreachable, err)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarx.Write(pw, h.Path)) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, pr)
	if err != nil {
		_ = pr.Close()
		return failed(stderr, ExitFailed, err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.Do(req)
	_ = pr.Close()
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return writeAnswer(stdout, answerOf(resp))
}
