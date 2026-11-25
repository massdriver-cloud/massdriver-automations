// autodeploy-downstream receives Massdriver deployment.completed events and deploys
// every downstream instance that carries the opt-in attribute.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	signatureTolerance = 5 * time.Minute
	maxBodyBytes       = 1 << 20
	pageLimit          = 100
)

type config struct {
	url           string
	orgID         string
	apiKey        string
	signingSecret []byte
	attrKey       string
	attrValue     string
	port          string
}

func loadConfig() (config, error) {
	c := config{
		url:       env("MASSDRIVER_URL", "https://api.massdriver.cloud"),
		orgID:     os.Getenv("MASSDRIVER_ORGANIZATION_ID"),
		apiKey:    os.Getenv("MASSDRIVER_API_KEY"),
		attrKey:   env("AUTO_DEPLOY_ATTR_KEY", "autodeploy"),
		attrValue: env("AUTO_DEPLOY_ATTR_VALUE", "true"),
		port:      env("PORT", "8080"),
	}

	var missing []string
	for name, v := range map[string]string{
		"MASSDRIVER_ORGANIZATION_ID": c.orgID,
		"MASSDRIVER_API_KEY":         c.apiKey,
		"MASSDRIVER_SIGNING_SECRET":  os.Getenv("MASSDRIVER_SIGNING_SECRET"),
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}

	secret, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(os.Getenv("MASSDRIVER_SIGNING_SECRET"), "whsec_"))
	if err != nil {
		return c, fmt.Errorf("MASSDRIVER_SIGNING_SECRET is not a whsec_ secret: %w", err)
	}
	c.signingSecret = secret
	return c, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	r := &receiver{cfg: cfg, api: &client{url: strings.TrimRight(cfg.url, "/") + "/api/v2", apiKey: cfg.apiKey}, seen: map[string]time.Time{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", r.handle)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	slog.Info("listening", "port", cfg.port, "attr_key", cfg.attrKey, "attr_value", cfg.attrValue)
	if err := http.ListenAndServe(":"+cfg.port, mux); err != nil {
		slog.Error("server", "error", err)
		os.Exit(1)
	}
}

// --- webhook ---------------------------------------------------------------

type event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Entity struct {
			ID string `json:"id"`
		} `json:"entity"`
		Attributes map[string]any `json:"attributes"`
		Metadata   struct {
			DeploymentAction string `json:"deployment_action"`
		} `json:"metadata"`
	} `json:"data"`
}

type receiver struct {
	cfg config
	api *client

	mu   sync.Mutex
	seen map[string]time.Time
}

func (r *receiver) handle(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !verify(r.cfg.signingSecret, req.Header, body) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}

	upstream, _ := ev.Data.Attributes["md-instance"].(string)
	if ev.Type != "deployment.completed" || ev.Data.Metadata.DeploymentAction != "PROVISION" || upstream == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.alreadySeen(ev.ID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Massdriver waits 10 seconds for a response, and a cascade can take longer.
	w.WriteHeader(http.StatusAccepted)
	go r.deployDownstream(ev, upstream)
}

// Massdriver delivers at least once; a redelivered event must not start a second round of deploys.
func (r *receiver) alreadySeen(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, t := range r.seen {
		if now.Sub(t) > time.Hour {
			delete(r.seen, k)
		}
	}
	if _, ok := r.seen[id]; ok {
		return true
	}
	r.seen[id] = now
	return false
}

// Standard Webhooks: base64 HMAC-SHA256 of "<id>.<timestamp>.<body>".
func verify(secret []byte, h http.Header, body []byte) bool {
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if id == "" || sigs == "" || err != nil {
		return false
	}
	if d := time.Since(time.Unix(sec, 0)); d > signatureTolerance || d < -signatureTolerance {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	for _, sig := range strings.Fields(sigs) {
		if v, ok := strings.CutPrefix(sig, "v1,"); ok && hmac.Equal([]byte(v), []byte(expected)) {
			return true
		}
	}
	return false
}

// --- downstream ------------------------------------------------------------

type instance struct {
	ID                  string          `json:"id"`
	Status              string          `json:"status"`
	Params              json.RawMessage `json:"params"`
	EffectiveAttributes map[string]any  `json:"effectiveAttributes"`
	Dependencies        []struct {
		Resource struct {
			ID string `json:"id"`
		} `json:"resource"`
	} `json:"dependencies"`
}

const instanceFields = `id status params effectiveAttributes dependencies { resource { id } }`

func (r *receiver) deployDownstream(ev event, upstream string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.With("event_id", ev.ID, "upstream", upstream)

	downstream, err := r.findDownstream(ctx, upstream)
	if err != nil {
		log.Error("find downstream", "error", err)
		return
	}

	for _, inst := range downstream {
		l := log.With("instance", inst.ID)
		if fmt.Sprint(inst.EffectiveAttributes[r.cfg.attrKey]) != r.cfg.attrValue {
			l.Info("skip: not opted in")
			continue
		}
		if inst.Status != "PROVISIONED" {
			l.Info("skip: not provisioned", "status", inst.Status)
			continue
		}
		msg := fmt.Sprintf("Auto-deploy: upstream %s completed deployment %s", upstream, ev.Data.Entity.ID)
		id, err := r.deploy(ctx, inst, msg)
		if err != nil {
			l.Error("deploy", "error", err)
			continue
		}
		l.Info("deployed", "deployment_id", id)
	}
}

// An instance is downstream when one of its dependencies resolves to a resource the upstream produces.
// Candidates come from all three ways a dependency is filled: links and environment defaults in the
// upstream's environment, environment defaults in other environments, and remote references.
func (r *receiver) findDownstream(ctx context.Context, upstream string) ([]instance, error) {
	var up struct {
		Instance struct {
			Environment struct {
				ID string `json:"id"`
			} `json:"environment"`
			Resources []struct {
				Resource struct {
					ID string `json:"id"`
				} `json:"resource"`
			} `json:"resources"`
		} `json:"instance"`
	}
	err := r.api.do(ctx, `query($org: ID!, $id: ID!) {
		instance(organizationId: $org, id: $id) { environment { id } resources { resource { id } } }
	}`, map[string]any{"org": r.cfg.orgID, "id": upstream}, &up)
	if err != nil {
		return nil, err
	}

	produced := map[string]bool{}
	environments := map[string]bool{up.Instance.Environment.ID: true}
	candidates := map[string]instance{}

	for _, res := range up.Instance.Resources {
		produced[res.Resource.ID] = true

		refs, envs, err := r.resourceConsumers(ctx, res.Resource.ID)
		if err != nil {
			return nil, err
		}
		for _, inst := range refs {
			candidates[inst.ID] = inst
		}
		for _, env := range envs {
			environments[env] = true
		}
	}

	for env := range environments {
		insts, err := r.environmentInstances(ctx, env)
		if err != nil {
			return nil, err
		}
		for _, inst := range insts {
			candidates[inst.ID] = inst
		}
	}

	var downstream []instance
	for _, inst := range candidates {
		if inst.ID == upstream {
			continue
		}
		for _, dep := range inst.Dependencies {
			if produced[dep.Resource.ID] {
				downstream = append(downstream, inst)
				break
			}
		}
	}
	return downstream, nil
}

type cursor struct {
	Next *string `json:"next"`
}

func (r *receiver) resourceConsumers(ctx context.Context, resourceID string) ([]instance, []string, error) {
	var insts []instance
	var envs []string
	var refsNext, defaultsNext *string
	refsDone, defaultsDone := false, false

	for !refsDone || !defaultsDone {
		var out struct {
			Resource struct {
				RemoteReferences struct {
					Cursor cursor `json:"cursor"`
					Items  []struct {
						Instance instance `json:"instance"`
					} `json:"items"`
				} `json:"remoteReferences"`
				EnvironmentDefaults struct {
					Cursor cursor `json:"cursor"`
					Items  []struct {
						Environment struct {
							ID string `json:"id"`
						} `json:"environment"`
					} `json:"items"`
				} `json:"environmentDefaults"`
			} `json:"resource"`
		}
		err := r.api.do(ctx, `query($org: ID!, $id: ID!, $refs: Cursor, $defaults: Cursor) {
			resource(organizationId: $org, id: $id) {
				remoteReferences(cursor: $refs) { cursor { next } items { instance { `+instanceFields+` } } }
				environmentDefaults(cursor: $defaults) { cursor { next } items { environment { id } } }
			}
		}`, map[string]any{
			"org":      r.cfg.orgID,
			"id":       resourceID,
			"refs":     map[string]any{"limit": pageLimit, "next": refsNext},
			"defaults": map[string]any{"limit": pageLimit, "next": defaultsNext},
		}, &out)
		if err != nil {
			return nil, nil, err
		}

		if !refsDone {
			for _, item := range out.Resource.RemoteReferences.Items {
				insts = append(insts, item.Instance)
			}
			refsNext = out.Resource.RemoteReferences.Cursor.Next
			refsDone = refsNext == nil
		}
		if !defaultsDone {
			for _, item := range out.Resource.EnvironmentDefaults.Items {
				envs = append(envs, item.Environment.ID)
			}
			defaultsNext = out.Resource.EnvironmentDefaults.Cursor.Next
			defaultsDone = defaultsNext == nil
		}
	}
	return insts, envs, nil
}

func (r *receiver) environmentInstances(ctx context.Context, envID string) ([]instance, error) {
	var out struct {
		Environment struct {
			Instances []instance `json:"instances"`
		} `json:"environment"`
	}
	err := r.api.do(ctx, `query($org: ID!, $id: ID!) {
		environment(organizationId: $org, id: $id) { instances { `+instanceFields+` } }
	}`, map[string]any{"org": r.cfg.orgID, "id": envID}, &out)
	return out.Environment.Instances, err
}

func (r *receiver) deploy(ctx context.Context, inst instance, message string) (string, error) {
	params := string(inst.Params)
	if params == "" || params == "null" {
		params = "{}"
	}

	var out struct {
		CreateDeployment struct {
			Successful bool `json:"successful"`
			Messages   []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"messages"`
			Result struct {
				ID string `json:"id"`
			} `json:"result"`
		} `json:"createDeployment"`
	}
	err := r.api.do(ctx, `mutation($org: ID!, $id: ID!, $input: CreateDeploymentInput!) {
		createDeployment(organizationId: $org, id: $id, input: $input) {
			successful messages { field message } result { id }
		}
	}`, map[string]any{
		"org":   r.cfg.orgID,
		"id":    inst.ID,
		"input": map[string]any{"action": "PROVISION", "params": params, "message": message},
	}, &out)
	if err != nil {
		return "", err
	}
	if !out.CreateDeployment.Successful {
		b, _ := json.Marshal(out.CreateDeployment.Messages)
		return "", fmt.Errorf("createDeployment refused: %s", b)
	}
	return out.CreateDeployment.Result.ID, nil
}

// --- graphql ---------------------------------------------------------------

type client struct {
	url    string
	apiKey string
}

func (c *client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("graphql: HTTP %d: %s", resp.StatusCode, b)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}
		return errors.New("graphql: " + strings.Join(msgs, "; "))
	}
	return json.Unmarshal(envelope.Data, out)
}
