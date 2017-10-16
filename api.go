package spamc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Command types.
const (
	CmdSymbols      = "SYMBOLS"
	CmdReport       = "REPORT"
	CmdReportIfspam = "REPORT_IFSPAM"
	CmdSkip         = "SKIP"
	CmdPing         = "PING"
	CmdTell         = "TELL"
	CmdProcess      = "PROCESS"
	CmdHeaders      = "HEADERS"
)

// Learn types.
const (
	LearnSpam   = "SPAM"
	LearnHam    = "HAM"
	LearnForget = "FORGET"
)

// Client is a connection to the spamd daemon.
type Client struct {
	// DefaultUser is the User to send if a command didn't specify one.
	DefaultUser string

	host   string
	dialer net.Dialer
	conn   net.Conn
}

// Response is the default response struct.
type Response struct {
	Message string
	Vars    map[string]interface{}
}

// Error is used for spamd responses; it contains the spamd exit code.
type Error struct {
	msg  string
	Code int64
	Line string
}

func (e Error) Error() string { return e.msg }

// New instance of Client.
func New(host string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Client{
		host: host,
		dialer: net.Dialer{
			Timeout: timeout,
		},
	}
}

// NewWithDialer creates a new instance of Client.
func NewWithDialer(host string, dialer net.Dialer) *Client {
	return &Client{
		host:   host,
		dialer: dialer,
	}
}

// CheckResponse is the response from the Check command.
type CheckResponse struct {
	//Response

	// IsSpam reports if this message is considered spam.
	IsSpam bool

	// Score is the spam score of this message.
	Score float64

	// BaseScore is the "minimum spam score" configured on the server. This
	// is usually 5.0.
	BaseScore float64
}

// Check if the passed message is spam.
func (c *Client) Check(
	ctx context.Context,
	msg string, headers Header,
) (*CheckResponse, error) {

	read, err := c.send(ctx, "CHECK", msg, headers)
	if err != nil {
		return nil, fmt.Errorf("error sending command to spamd: %v", err)
	}
	defer read.Close() // nolint: errcheck

	respHeaders, _, err := readResponse(read)
	if err != nil {
		return nil, fmt.Errorf("could not parse spamd response: %v", err)
	}

	// Spam <yes|no> <score> / <base-score>
	// Spam: yes; 6.66 / 5.0
	spam, ok := respHeaders["Spam"]
	if !ok || len(spam) == 0 {
		return nil, errors.New("Spam header missing in response")
	}

	r := CheckResponse{}
	s := strings.Split(spam[0], ";")
	if len(s) != 2 {
		return nil, fmt.Errorf("unexpected data: %v", spam[0])
	}

	switch strings.ToLower(strings.TrimSpace(s[0])) {
	case "true", "yes":
		r.IsSpam = true
	case "false", "no":
		r.IsSpam = false
	default:
		return nil, fmt.Errorf("unknown spam status: %v", s[0])
	}

	score := strings.Split(s[1], "/")
	if len(score) != 2 {
		return nil, fmt.Errorf("unexpected data: %v", s[1])
	}
	r.Score, err = strconv.ParseFloat(strings.TrimSpace(score[0]), 64)
	if err != nil {
		return nil, fmt.Errorf("could not parse spam score: %v", err)
	}
	r.BaseScore, err = strconv.ParseFloat(strings.TrimSpace(score[1]), 64)
	if err != nil {
		return nil, fmt.Errorf("could not parse base spam score: %v", err)
	}

	return &r, nil
}

// Symbols check if message is spam and return the score and a list of all
// symbols that were hit.
func (c *Client) Symbols(
	ctx context.Context,
	msg string,
	headers Header,
) (*Response, error) {
	return c.simpleCall(CmdSymbols, msg, headers)
}

// Report checks if the message is spam and returns the score plus report.
func (c *Client) Report(
	ctx context.Context,
	msg string,
	headers Header,
) (*Response, error) {
	return c.simpleCall(CmdReport, msg, headers)
}

// ReportIfSpam checks if the message is spam and returns the score plus report
// if the message is spam.
func (c *Client) ReportIfSpam(
	ctx context.Context,
	msg string,
	headers Header,
) (*Response, error) {
	return c.simpleCall(CmdReportIfspam, msg, headers)
}

// Skip ignores this message: client opened connection then changed its mind.
func (c *Client) Skip(
	ctx context.Context,
	msg string,
	headers Header,
) (*Response, error) {
	return c.simpleCall(CmdSkip, msg, headers)
}

// Ping returns a confirmation that spamd is alive.
func (c *Client) Ping(ctx context.Context) (*Response, error) {
	return c.simpleCall(CmdPing, "", nil)
}

// Process this message and return a modified message.
func (c *Client) Process(
	ctx context.Context,
	msg string,
	headers Header,
) (*Response, error) {
	return c.simpleCall(CmdProcess, msg, headers)
}

// Tell what type of we are to process and what should be done with that
// message.
//
// This includes setting or removing a local or a remote database (learning,
// reporting, forgetting, revoking).
func (c *Client) Tell(
	ctx context.Context,
	msg string,
	headers Header,
) (*Response, error) {
	read, err := c.call(CmdTell, msg, headers)
	defer read.Close() // nolint: errcheck
	if err != nil {
		return nil, err
	}

	r, err := processResponse(CmdTell, read)
	if err != nil {
		if serr, ok := err.(Error); ok && serr.Code == 69 {
			return nil, errors.New(
				"TELL commands are not enabled, set the --allow-tell switch")
		}

		return nil, err
	}

	return r, nil
}

// Headers is the same as Process() but returns only modified headers and not
// the body.
func (c *Client) Headers(
	ctx context.Context,
	msg string,
	headers Header,
) (*Response, error) {
	return c.simpleCall(CmdHeaders, msg, headers)
}

// Learn if a message is spam. This is a more convenient wrapper around SA's
// "TELL" command.
//
// Use one of the Learn* constants as the learnType.
func (c *Client) Learn(
	ctx context.Context,
	learnType, msg string,
	headers Header,
) (*Response, error) {

	if headers == nil {
		headers = make(Header)
	}
	switch strings.ToUpper(learnType) {
	case LearnSpam:
		headers.Add(HeaderMessageClass, "spam")
		headers.Add(HeaderSet, "local")
	case LearnHam:
		headers.Add(HeaderMessageClass, "ham")
		headers.Add(HeaderSet, "local")
	case LearnForget:
		headers.Add(HeaderRemove, "local")
	default:
		return nil, fmt.Errorf("unknown learn type: %v", learnType)
	}
	return c.Tell(ctx, msg, headers)
}

// Send a command a SpamAssassin.
func (c *Client) Send(
	ctx context.Context,
	cmd, msg string,
	headers Header,
) (*Response, error) {
	return c.simpleCall(strings.ToUpper(cmd), msg, headers)
}
