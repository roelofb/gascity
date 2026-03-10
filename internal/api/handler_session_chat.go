package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

var errSessionTemplateNotFound = errors.New("session template not found")

type sessionCreateRequest struct {
	Template string `json:"template"`
	Title    string `json:"title,omitempty"`
}

type sessionMessageRequest struct {
	Message string `json:"message"`
}

type sessionPendingResponse struct {
	Supported bool                        `json:"supported"`
	Pending   *runtime.PendingInteraction `json:"pending,omitempty"`
}

type sessionRespondRequest struct {
	RequestID string            `json:"request_id,omitempty"`
	Action    string            `json:"action"`
	Text      string            `json:"text,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type sessionTranscriptResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Format     string                     `json:"format"`
	Turns      []outputTurn               `json:"turns"`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

type sessionRawTranscriptResponse struct {
	ID         string                     `json:"id"`
	Template   string                     `json:"template"`
	Format     string                     `json:"format"`
	Messages   []json.RawMessage          `json:"messages"`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

func (s *Server) sessionLogPaths() []string {
	if s.sessionLogSearchPaths != nil {
		return s.sessionLogSearchPaths
	}
	cfg := s.state.Config()
	if cfg == nil {
		return sessionlog.DefaultSearchPaths()
	}
	return sessionlog.MergeSearchPaths(cfg.Daemon.ObservePaths)
}

func sessionCreateHints(resolved *config.ResolvedProvider) runtime.Config {
	return runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
	}
}

func sessionResumeHints(resolved *config.ResolvedProvider, workDir string) runtime.Config {
	return runtime.Config{
		WorkDir:                workDir,
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		Env:                    resolved.Env,
	}
}

func (s *Server) resolveSessionTemplate(template string) (*config.ResolvedProvider, string, string, string, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, "", "", "", errors.New("no city config loaded")
	}
	agentCfg, ok := resolveSessionTemplateAgent(cfg, template)
	if !ok {
		return nil, "", "", "", errSessionTemplateNotFound
	}
	resolved, err := config.ResolveProvider(&agentCfg, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return nil, "", "", "", err
	}
	workDir := s.resolveAgentWorkDir(agentCfg)
	if workDir == "" {
		workDir = s.state.CityPath()
	}
	return resolved, workDir, agentCfg.Session, agentCfg.QualifiedName(), nil
}

func (s *Server) buildSessionResume(info session.Info) (string, runtime.Config) {
	cmd := session.BuildResumeCommand(info)
	resolved, workDir, _, _, err := s.resolveSessionTemplate(info.Template)
	if err != nil {
		return cmd, runtime.Config{WorkDir: info.WorkDir}
	}
	if info.WorkDir != "" {
		workDir = info.WorkDir
	}
	return cmd, sessionResumeHints(resolved, workDir)
}

func writeSessionManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrInteractionUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", err.Error())
	case errors.Is(err, session.ErrPendingInteraction):
		writeError(w, http.StatusConflict, "pending_interaction", err.Error())
	case errors.Is(err, session.ErrTransportUnknown):
		writeError(w, http.StatusConflict, "unknown_transport", err.Error())
	case errors.Is(err, session.ErrNoPendingInteraction):
		writeError(w, http.StatusConflict, "no_pending", err.Error())
	case errors.Is(err, session.ErrInteractionMismatch):
		writeError(w, http.StatusConflict, "invalid_interaction", err.Error())
	case errors.Is(err, session.ErrSessionClosed), errors.Is(err, session.ErrResumeRequired):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, session.ErrNotSession):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	default:
		writeStoreError(w, err)
	}
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	var body sessionCreateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Template == "" {
		writeError(w, http.StatusBadRequest, "invalid", "template is required")
		return
	}

	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	resolved, workDir, transport, template, err := s.resolveSessionTemplate(body.Template)
	if err != nil {
		s.idem.unreserve(idemKey)
		if errors.Is(err, errSessionTemplateNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "template "+body.Template+" not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	title := body.Title
	if title == "" {
		title = template
	}

	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		SessionIDFlag: resolved.SessionIDFlag,
	}

	mgr := s.sessionManager(store)
	info, err := mgr.CreateWithTransport(
		r.Context(),
		template,
		title,
		resolved.CommandString(),
		workDir,
		resolved.Name,
		transport,
		resolved.Env,
		resume,
		sessionCreateHints(resolved),
	)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := sessionToResponse(info, s.state.Config())
	s.enrichSessionResponse(&resp, info, s.state.Config(), s.state.SessionProvider(), false)
	s.idem.storeResponse(idemKey, bodyHash, http.StatusCreated, resp)
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
	info, err := mgr.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}

	path, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}

	wantRaw := r.URL.Query().Get("format") == "raw"

	if path != "" {
		tail := 0
		if v := r.URL.Query().Get("tail"); v != "" {
			if n, convErr := strconv.Atoi(v); convErr == nil && n >= 0 {
				tail = n
			}
		}
		before := r.URL.Query().Get("before")

		var sess *sessionlog.Session
		if before != "" {
			sess, err = sessionlog.ReadFileOlder(path, tail, before)
		} else {
			sess, err = sessionlog.ReadFile(path, tail)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "reading session log: "+err.Error())
			return
		}

		if wantRaw {
			msgs := make([]json.RawMessage, 0, len(sess.Messages))
			for _, entry := range sess.Messages {
				switch entry.Type {
				case "user", "assistant", "system", "result":
					if len(entry.Raw) > 0 {
						msgs = append(msgs, entry.Raw)
					}
				}
			}
			writeJSON(w, http.StatusOK, sessionRawTranscriptResponse{
				ID:         info.ID,
				Template:   info.Template,
				Format:     "raw",
				Messages:   msgs,
				Pagination: sess.Pagination,
			})
			return
		}

		turns := make([]outputTurn, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			turn := entryToTurn(entry)
			if turn.Text == "" {
				continue
			}
			turns = append(turns, turn)
		}
		writeJSON(w, http.StatusOK, sessionTranscriptResponse{
			ID:         info.ID,
			Template:   info.Template,
			Format:     "conversation",
			Turns:      turns,
			Pagination: sess.Pagination,
		})
		return
	}

	if wantRaw {
		writeJSON(w, http.StatusOK, sessionRawTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "raw",
			Messages: []json.RawMessage{},
		})
		return
	}

	if info.State == session.StateActive && s.state.SessionProvider().IsRunning(info.SessionName) {
		output, peekErr := s.state.SessionProvider().Peek(info.SessionName, 100)
		if peekErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", peekErr.Error())
			return
		}
		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		writeJSON(w, http.StatusOK, sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "text",
			Turns:    turns,
		})
		return
	}

	writeJSON(w, http.StatusOK, sessionTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "conversation",
		Turns:    []outputTurn{},
	})
}

func (s *Server) handleSessionMessage(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	var body sessionMessageRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "invalid", "message is required")
		return
	}

	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	mgr := s.sessionManager(store)
	info, err := mgr.Get(id)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	resumeCommand, hints := s.buildSessionResume(info)
	if err := mgr.Send(r.Context(), id, body.Message, resumeCommand, hints); err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	resp := map[string]string{"status": "accepted", "id": id}
	s.idem.storeResponse(idemKey, bodyHash, http.StatusAccepted, resp)
	writeJSON(w, http.StatusAccepted, resp)
}

func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
	if err := mgr.StopTurn(id); err != nil {
		writeSessionManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "id": id})
}

func (s *Server) handleSessionPending(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
	pending, supported, err := mgr.Pending(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionPendingResponse{
		Supported: supported,
		Pending:   pending,
	})
}

func (s *Server) handleSessionRespond(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	var body sessionRespondRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Action == "" {
		writeError(w, http.StatusBadRequest, "invalid", "action is required")
		return
	}

	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	mgr := s.sessionManager(store)
	if err := mgr.Respond(id, runtime.InteractionResponse{
		RequestID: body.RequestID,
		Action:    body.Action,
		Text:      body.Text,
		Metadata:  body.Metadata,
	}); err != nil {
		s.idem.unreserve(idemKey)
		writeSessionManagerError(w, err)
		return
	}

	resp := map[string]string{"status": "accepted", "id": id}
	s.idem.storeResponse(idemKey, bodyHash, http.StatusAccepted, resp)
	writeJSON(w, http.StatusAccepted, resp)
}

func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	store := s.state.CityBeadStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no bead store configured")
		return
	}

	id, err := session.ResolveSessionID(store, r.PathValue("id"))
	if err != nil {
		writeResolveError(w, err)
		return
	}

	mgr := s.sessionManager(store)
	info, err := mgr.Get(id)
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}
	path, err := mgr.TranscriptPath(id, s.sessionLogPaths())
	if err != nil {
		writeSessionManagerError(w, err)
		return
	}

	sp := s.state.SessionProvider()
	running := info.State == session.StateActive && sp.IsRunning(info.SessionName)
	if path == "" && !running {
		writeError(w, http.StatusNotFound, "not_found", "session "+id+" has no live output")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if info.State != "" {
		w.Header().Set("GC-Session-State", string(info.State))
	}
	if !running {
		w.Header().Set("GC-Session-Status", "stopped")
	}
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}

	ctx := r.Context()
	format := r.URL.Query().Get("format")
	if info.Closed {
		if format == "raw" {
			s.emitClosedSessionSnapshotRaw(w, info, path)
		} else {
			s.emitClosedSessionSnapshot(w, info, path)
		}
		return
	}
	switch {
	case path != "":
		if format == "raw" {
			s.streamSessionTranscriptLogRaw(ctx, w, info, path)
		} else {
			s.streamSessionTranscriptLog(ctx, w, info, path)
		}
	default:
		s.streamSessionPeek(ctx, w, info)
	}
}

func (s *Server) emitClosedSessionSnapshot(w http.ResponseWriter, info session.Info, logPath string) {
	if logPath == "" {
		return
	}
	sess, err := sessionlog.ReadFile(logPath, 0)
	if err != nil {
		return
	}

	turns := make([]outputTurn, 0, len(sess.Messages))
	for _, entry := range sess.Messages {
		turn := entryToTurn(entry)
		if turn.Text == "" {
			continue
		}
		turns = append(turns, turn)
	}
	if len(turns) == 0 {
		return
	}

	data, err := json.Marshal(sessionTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "conversation",
		Turns:    turns,
	})
	if err != nil {
		return
	}
	writeSSE(w, "turn", 1, data)
}

func (s *Server) emitClosedSessionSnapshotRaw(w http.ResponseWriter, info session.Info, logPath string) {
	if logPath == "" {
		return
	}
	sess, err := sessionlog.ReadFile(logPath, 0)
	if err != nil {
		return
	}

	rawMessages := make([]json.RawMessage, 0, len(sess.Messages))
	for _, entry := range sess.Messages {
		if len(entry.Raw) == 0 {
			continue
		}
		rawMessages = append(rawMessages, entry.Raw)
	}
	if len(rawMessages) == 0 {
		return
	}

	data, err := json.Marshal(sessionRawTranscriptResponse{
		ID:       info.ID,
		Template: info.Template,
		Format:   "raw",
		Messages: rawMessages,
	})
	if err != nil {
		return
	}
	writeSSE(w, "message", 1, data)
}

func (s *Server) streamSessionTranscriptLogRaw(ctx context.Context, w http.ResponseWriter, info session.Info, logPath string) {
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	// Try fsnotify for real-time delivery; fall back to polling.
	watcher, watchErr := fsnotify.NewWatcher()
	var fallbackPoll *time.Ticker
	if watchErr == nil {
		if addErr := watcher.Add(logPath); addErr != nil {
			_ = watcher.Close()
			watcher = nil
		}
	} else {
		watcher = nil
	}
	if watcher != nil {
		defer watcher.Close() //nolint:errcheck // best-effort cleanup
	} else {
		fallbackPoll = time.NewTicker(outputStreamPollInterval)
		defer fallbackPoll.Stop()
		log.Printf("session stream: fsnotify unavailable for %s, falling back to polling", logPath)
	}

	var lastSize int64
	var lastSentUUID string
	var seq uint64

	readAndEmit := func() {
		stat, err := os.Stat(logPath)
		if err != nil {
			return
		}
		if stat.Size() == lastSize {
			return
		}

		sess, err := sessionlog.ReadFile(logPath, 0)
		if err != nil {
			return
		}
		lastSize = stat.Size()

		rawMessages := make([]json.RawMessage, 0, len(sess.Messages))
		uuids := make([]string, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			if len(entry.Raw) == 0 {
				continue
			}
			rawMessages = append(rawMessages, entry.Raw)
			uuids = append(uuids, entry.UUID)
		}
		if len(rawMessages) == 0 {
			return
		}

		startIdx := 0
		if lastSentUUID != "" {
			for i, uuid := range uuids {
				if uuid == lastSentUUID {
					startIdx = i + 1
					break
				}
			}
		}
		if startIdx >= len(rawMessages) {
			return
		}
		lastSentUUID = uuids[len(uuids)-1]
		seq++

		data, err := json.Marshal(sessionRawTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "raw",
			Messages: rawMessages[startIdx:],
		})
		if err != nil {
			return
		}
		writeSSE(w, "message", seq, data)
	}

	readAndEmit()

	for {
		if watcher != nil {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Write) {
					readAndEmit()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("fsnotify: watcher error: %v, switching to polling", err)
				watcher.Close() //nolint:errcheck // best-effort cleanup on error transition
				watcher = nil
				fallbackPoll = time.NewTicker(outputStreamPollInterval)
				defer fallbackPoll.Stop() //nolint:staticcheck // deferred in loop is intentional; executed on function return
			case <-keepalive.C:
				writeSSEComment(w)
			}
		} else {
			select {
			case <-ctx.Done():
				return
			case <-fallbackPoll.C:
				readAndEmit()
			case <-keepalive.C:
				writeSSEComment(w)
			}
		}
	}
}

func (s *Server) streamSessionTranscriptLog(ctx context.Context, w http.ResponseWriter, info session.Info, logPath string) {
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	// Try fsnotify for real-time delivery; fall back to polling.
	watcher, watchErr := fsnotify.NewWatcher()
	var fallbackPoll *time.Ticker
	if watchErr == nil {
		if addErr := watcher.Add(logPath); addErr != nil {
			_ = watcher.Close()
			watcher = nil
		}
	} else {
		watcher = nil
	}
	if watcher != nil {
		defer watcher.Close() //nolint:errcheck // best-effort cleanup
	} else {
		fallbackPoll = time.NewTicker(outputStreamPollInterval)
		defer fallbackPoll.Stop()
		log.Printf("session stream: fsnotify unavailable for %s, falling back to polling", logPath)
	}

	var lastSize int64
	var lastSentUUID string
	var seq uint64

	readAndEmit := func() {
		stat, err := os.Stat(logPath)
		if err != nil {
			return
		}
		if stat.Size() == lastSize {
			return
		}

		sess, err := sessionlog.ReadFile(logPath, 0)
		if err != nil {
			return
		}
		lastSize = stat.Size()

		turns := make([]outputTurn, 0, len(sess.Messages))
		uuids := make([]string, 0, len(sess.Messages))
		for _, entry := range sess.Messages {
			turn := entryToTurn(entry)
			if turn.Text == "" {
				continue
			}
			turns = append(turns, turn)
			uuids = append(uuids, entry.UUID)
		}
		if len(turns) == 0 {
			return
		}

		startIdx := 0
		if lastSentUUID != "" {
			for i, uuid := range uuids {
				if uuid == lastSentUUID {
					startIdx = i + 1
					break
				}
			}
		}
		if startIdx >= len(turns) {
			return
		}
		lastSentUUID = uuids[len(uuids)-1]
		seq++

		data, err := json.Marshal(sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "conversation",
			Turns:    turns[startIdx:],
		})
		if err != nil {
			return
		}
		writeSSE(w, "turn", seq, data)
	}

	readAndEmit()

	for {
		if watcher != nil {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Write) {
					readAndEmit()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("fsnotify: watcher error: %v, switching to polling", err)
				watcher.Close() //nolint:errcheck // best-effort cleanup on error transition
				watcher = nil
				fallbackPoll = time.NewTicker(outputStreamPollInterval)
				defer fallbackPoll.Stop() //nolint:staticcheck // deferred in loop is intentional; executed on function return
			case <-keepalive.C:
				writeSSEComment(w)
			}
		} else {
			select {
			case <-ctx.Done():
				return
			case <-fallbackPoll.C:
				readAndEmit()
			case <-keepalive.C:
				writeSSEComment(w)
			}
		}
	}
}

func (s *Server) streamSessionPeek(ctx context.Context, w http.ResponseWriter, info session.Info) {
	sp := s.state.SessionProvider()
	poll := time.NewTicker(outputStreamPollInterval)
	defer poll.Stop()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	var lastOutput string
	var seq uint64

	emitPeek := func() {
		if !sp.IsRunning(info.SessionName) {
			return
		}
		output, err := sp.Peek(info.SessionName, 100)
		if err != nil || output == lastOutput {
			return
		}
		lastOutput = output
		seq++

		turns := []outputTurn{}
		if output != "" {
			turns = append(turns, outputTurn{Role: "output", Text: output})
		}
		data, err := json.Marshal(sessionTranscriptResponse{
			ID:       info.ID,
			Template: info.Template,
			Format:   "text",
			Turns:    turns,
		})
		if err != nil {
			return
		}
		writeSSE(w, "turn", seq, data)
	}

	emitPeek()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			emitPeek()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}
