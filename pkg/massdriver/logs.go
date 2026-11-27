package massdriver

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

//go:embed queries/deployment_logs.graphql
var deploymentLogsQuery string

// ErrDeploymentNotFound is a deployment that the API does not return, because it does not exist or the service
// account cannot view it.
var ErrDeploymentNotFound = errors.New("deployment not found")

// DeploymentLogs writes the logs of a deployment to w, oldest first, with a newline after each batch. It returns the
// number of bytes written, and stops reading after limit bytes.
//
// The response is decoded as it arrives: the logs of a long deployment can be tens of megabytes, and a caller that
// scans w line by line never holds all of them.
func (c *Client) DeploymentLogs(ctx context.Context, id string, w io.Writer, limit int64) (written int64, truncated bool, err error) {
	resp, err := c.post(ctx, deploymentLogsQuery, map[string]any{"org": c.OrganizationID, "id": id})
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)
	var gqlErrs graphQLErrors
	found, done := false, false
	err = eachKey(dec, func(key string) error {
		switch {
		case done:
			return skip(dec)
		case key == "errors":
			return dec.Decode(&gqlErrs)
		case key != "data":
			return skip(dec)
		}
		return eachKey(dec, func(key string) error {
			if key != "deployment" {
				return skip(dec)
			}
			return eachKey(dec, func(key string) error {
				found = true
				if key != "logs" {
					return skip(dec)
				}
				if err := expectDelim(dec, '['); err != nil {
					return err
				}
				for dec.More() {
					var batch struct {
						Message string `json:"message"`
					}
					if err := dec.Decode(&batch); err != nil {
						return err
					}
					text := batch.Message + "\n"
					if rest := limit - written; int64(len(text)) > rest {
						text, truncated = text[:rest], true
					}
					n, err := io.WriteString(w, text)
					written += int64(n)
					if err != nil {
						return err
					}
					if truncated {
						done = true
						return errStop
					}
				}
				_, err := dec.Token()
				return err
			})
		})
	})
	if errors.Is(err, errStop) {
		return written, true, nil
	}
	if err != nil {
		return written, false, fmt.Errorf("deployment logs: %w", err)
	}
	if err := gqlErrs.err(); err != nil {
		return written, false, err
	}
	if !found {
		return 0, false, ErrDeploymentNotFound
	}
	return written, false, nil
}

var errStop = errors.New("stop")

// eachKey reads one JSON object and calls fn for each key; fn must read the key's value. A null is an object
// without keys.
func eachKey(dec *json.Decoder, fn func(key string) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("want an object, got %T", tok)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if err := fn(tok.(string)); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("want %q, got %T", want, tok)
	}
	return nil
}

func skip(dec *json.Decoder) error {
	var v json.RawMessage
	return dec.Decode(&v)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// StripANSI removes the terminal colors that provisioners write, such as OpenTofu's bold `Plan:`.
func StripANSI(s string) string { return ansi.ReplaceAllString(s, "") }
