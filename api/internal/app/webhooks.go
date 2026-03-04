package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"purl/api/internal/ratelimit"
)

// ── Zendesk webhook payload types ────────────────────────────────────────────

// flexInt64 unmarshals both numeric and quoted-string JSON integers.
// Zendesk event subscription webhook payloads send numeric IDs as strings
// (e.g. "id": "12345"), unlike the Zendesk REST API which uses bare numbers.
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(b []byte) error {
	var i int64
	if err := json.Unmarshal(b, &i); err == nil {
		*n = flexInt64(i)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("flexInt64: expected number or string, got %s", b)
	}
	parsed, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("flexInt64: parse %q: %w", s, err)
	}
	*n = flexInt64(parsed)
	return nil
}

type zendeskWebhookEnvelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	AccountID int64           `json:"account_id"`
	Detail    json.RawMessage `json:"detail"`
}

type webhookTicketDetail struct {
	ID          flexInt64  `json:"id"`
	Subject     string     `json:"subject"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	RequesterID flexInt64  `json:"requester_id"`
	AssigneeID  *flexInt64 `json:"assignee_id"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type webhookTicketDeletedDetail struct {
	ID flexInt64 `json:"id"`
}

type webhookCommentVia struct {
	Channel string `json:"channel"`
}

type webhookCommentDetail struct {
	ID        flexInt64         `json:"id"`
	TicketID  flexInt64         `json:"ticket_id"`
	AuthorID  flexInt64         `json:"author_id"`
	Type      string            `json:"type"`
	Body      string            `json:"body"`
	HtmlBody  string            `json:"html_body"`
	Public    bool              `json:"public"`
	Via       webhookCommentVia `json:"via"`
	CreatedAt time.Time         `json:"created_at"`
	Data      *zendeskVoiceData `json:"data"`
}

type webhookUserDetail struct {
	ID        flexInt64 `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Role      string    `json:"role"` // "end-user", "agent", "admin"
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ── HTTP ingestion handler ────────────────────────────────────────────────────

// @Summary     Zendesk webhook receiver
// @Tags        Webhooks
// @Description Receives Zendesk event subscription webhooks, verifies the
// @Description signature, and stores the raw payload for async processing.
// @Accept      json
// @Param       orgSlug  path  string  true  "Organization slug"
// @Success     204
// @Failure     400  {string}  string  "Bad Request"
// @Failure     401  {string}  string  "Unauthorized"
// @Failure     404  {string}  string  "Not Found"
// @Router      /webhooks/zendesk/{orgSlug} [post]
func (a *App) handleZendeskWebhook(w http.ResponseWriter, r *http.Request) {
	orgSlug := chi.URLParam(r, "orgSlug")

	// Buffer body before any DB work; needed for signature verification.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	// Load org and webhook secret.
	var orgID, webhookSecret string
	err = a.db.QueryRowContext(r.Context(),
		`SELECT id, COALESCE(zendesk_webhook_secret, '') FROM organizations WHERE slug = $1`,
		orgSlug,
	).Scan(&orgID, &webhookSecret)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		log.Printf("webhook: query org %q: %v", orgSlug, err)
		return
	}
	if webhookSecret == "" {
		http.Error(w, "webhook not configured", http.StatusUnauthorized)
		return
	}

	// Verify bearer token. Zendesk is configured to send the webhook secret
	// as "Authorization: Bearer <secret>". The scheme name is case-insensitive
	// per RFC 7235, so accept any casing.
	auth := r.Header.Get("Authorization")
	tokenOK := strings.EqualFold(auth[:min(len(auth), 7)], "bearer ") && auth[7:] == webhookSecret
	if !tokenOK {
		http.Error(w, "invalid authorization", http.StatusUnauthorized)
		return
	}

	// Parse only the envelope fields we need to store; leave detail opaque.
	var envelope zendeskWebhookEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// Store the raw payload — processing happens asynchronously via
	// the process-zendesk-webhooks command.
	_, err = a.db.ExecContext(r.Context(), `
		INSERT INTO zendesk_webhook_events (org_id, event_id, event_type, payload)
		VALUES ($1, $2, $3, $4)`,
		orgID, envelope.ID, envelope.Type, body,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		log.Printf("webhook: store event %q for org %q: %v", envelope.ID, orgSlug, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Async processing ──────────────────────────────────────────────────────────

// ProcessPendingWebhooks fetches all unprocessed webhook events from the
// zendesk_webhook_events table and processes them in arrival order. It marks
// each successfully processed event with the current timestamp. Events that fail
// processing are left with processed_at = NULL so they are retried on the next
// call. Unsupported event types are silently acknowledged and also marked as
// processed to prevent them from accumulating.
//
// limiter may be nil to skip rate limiting. Returns the number of events marked as processed.
func ProcessPendingWebhooks(ctx context.Context, db *sql.DB, limiter *ratelimit.Limiter) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, org_id, event_type, payload
		FROM zendesk_webhook_events
		WHERE processed_at IS NULL
		ORDER BY created_at ASC
		LIMIT 1000`,
	)
	if err != nil {
		return 0, fmt.Errorf("query pending events: %w", err)
	}

	type pending struct {
		id        string
		orgID     string
		eventType string
		payload   []byte
	}
	var events []pending
	for rows.Next() {
		var e pending
		if err := rows.Scan(&e.id, &e.orgID, &e.eventType, &e.payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate events: %w", err)
	}

	processed := 0
	for _, e := range events {
		if err := processZendeskEvent(ctx, db, e.orgID, e.eventType, e.payload, limiter); err != nil {
			log.Printf("process-zendesk-webhooks: event %s (%s) failed: %v — will retry", e.id, e.eventType, err)
			if _, dbErr := db.ExecContext(ctx,
				`UPDATE zendesk_webhook_events SET last_error = $1 WHERE id = $2`,
				err.Error(), e.id,
			); dbErr != nil {
				log.Printf("process-zendesk-webhooks: record error for event %s: %v", e.id, dbErr)
			}
			continue
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE zendesk_webhook_events SET processed_at = now(), last_error = NULL WHERE id = $1`,
			e.id,
		); err != nil {
			log.Printf("process-zendesk-webhooks: mark event %s processed: %v", e.id, err)
			continue
		}
		processed++
	}
	return processed, nil
}

// processZendeskEvent dispatches a single webhook event to the appropriate
// handler based on event_type. Unknown types are silently ignored (return nil)
// so the caller can mark them as processed without logging noise.
func processZendeskEvent(ctx context.Context, db *sql.DB, orgID, eventType string, payload []byte, limiter *ratelimit.Limiter) error {
	var envelope zendeskWebhookEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("unmarshal envelope: %w", err)
	}

	switch eventType {
	case "zen:event-type:ticket.created",
		"zen:event-type:ticket.updated",
		"zen:event-type:ticket.subject_changed",
		"zen:event-type:ticket.description_changed",
		"zen:event-type:ticket.agent_assignment_changed",
		"zen:event-type:ticket.merged":
		var d webhookTicketDetail
		if err := json.Unmarshal(envelope.Detail, &d); err != nil {
			return fmt.Errorf("unmarshal ticket detail: %w", err)
		}
		return handleTicketUpsert(ctx, db, orgID, &d, limiter)

	case "zen:event-type:ticket.comment_added", "zen:event-type:messaging_ticket.message_added":
		// Comment data in these events is incomplete (no html_body, no created_at),
		// so sync all comments from the REST API to get full fidelity data.
		var d struct {
			ID flexInt64 `json:"id"`
		}
		if err := json.Unmarshal(envelope.Detail, &d); err != nil {
			return fmt.Errorf("unmarshal ticket detail: %w", err)
		}
		return handleTicketCommentAdded(ctx, db, orgID, d.ID, limiter)

	case "zen:event-type:ticket.status_changed":
		var d webhookTicketDetail
		if err := json.Unmarshal(envelope.Detail, &d); err != nil {
			return fmt.Errorf("unmarshal ticket detail: %w", err)
		}
		return handleTicketStatusChanged(ctx, db, orgID, &d)

	case "zen:event-type:ticket.deleted",
		"zen:event-type:ticket.soft_deleted",
		"zen:event-type:ticket.permanently_deleted":
		var d webhookTicketDeletedDetail
		if err := json.Unmarshal(envelope.Detail, &d); err != nil {
			return fmt.Errorf("unmarshal ticket deleted detail: %w", err)
		}
		return handleTicketDeleted(ctx, db, orgID, d.ID)

	case "zen:event-type:comment.created":
		var d webhookCommentDetail
		if err := json.Unmarshal(envelope.Detail, &d); err != nil {
			return fmt.Errorf("unmarshal comment detail: %w", err)
		}
		return handleCommentCreated(ctx, db, orgID, &d, limiter)

	case "zen:event-type:comment.updated":
		var d webhookCommentDetail
		if err := json.Unmarshal(envelope.Detail, &d); err != nil {
			return fmt.Errorf("unmarshal comment detail: %w", err)
		}
		return handleCommentUpdated(ctx, db, orgID, &d, limiter)

	case "zen:event-type:user.created", "zen:event-type:user.updated":
		var d webhookUserDetail
		if err := json.Unmarshal(envelope.Detail, &d); err != nil {
			return fmt.Errorf("unmarshal user detail: %w", err)
		}
		return handleUserUpsert(ctx, db, orgID, &d)

	default:
		// Silently ignore unsupported event types; nil causes the caller to
		// mark the event as processed so it does not accumulate.
		return nil
	}
}

// ── Event handlers ────────────────────────────────────────────────────────────

func handleTicketUpsert(ctx context.Context, db *sql.DB, orgID string, d *webhookTicketDetail, limiter *ratelimit.Limiter) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Resolve reporter customer. If missing, fetch from Zendesk and upsert.
	reporterID, err := resolveOrFetchCustomer(ctx, db, tx, orgID, d.RequesterID, limiter)
	if err != nil {
		return fmt.Errorf("resolve reporter: %w", err)
	}
	if reporterID == "" {
		return fmt.Errorf("ticket %d: reporter %d not found and could not be fetched", d.ID, d.RequesterID)
	}

	// Resolve optional assignee agent. If not in the DB, try the Zendesk API.
	var assigneeID *string
	if d.AssigneeID != nil {
		var agentID string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM agents WHERE org_id = $1 AND zendesk_user_id = $2`,
			orgID, *d.AssigneeID,
		).Scan(&agentID); err == nil {
			assigneeID = &agentID
		} else {
			fetched, fetchErr := fetchZendeskUser(ctx, db, orgID, *d.AssigneeID, limiter)
			if fetchErr != nil {
				return fmt.Errorf("fetch assignee %d: %w", *d.AssigneeID, fetchErr)
			}
			if fetched != nil && fetched.Role != "end-user" {
				id, upsertErr := upsertAgent(ctx, tx, orgID, fetched.ID, fetched.Name, fetched.Email)
				if upsertErr != nil {
					return fmt.Errorf("upsert assignee agent: %w", upsertErr)
				}
				assigneeID = &id
			}
			// If credentials aren't configured or user isn't an agent, leave assignee_id NULL.
		}
	}

	// Capture old zendesk_status so we can detect changes for kanban sync.
	var oldStatus *string
	_ = tx.QueryRowContext(ctx,
		`SELECT zendesk_status::text FROM tickets WHERE org_id = $1 AND zendesk_ticket_id = $2`,
		orgID, d.ID,
	).Scan(&oldStatus)

	newStatus := mapZendeskStatus(d.Status)

	var ticketID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO tickets (title, description, reporter_id, assignee_id, org_id,
		                     zendesk_status, zendesk_ticket_id, received_at, zendesk_updated_at, resolved_at)
		VALUES ($1, $2, $3, $4, $5, $6::zendesk_status_category, $7, $8, $9,
		        CASE WHEN $6::zendesk_status_category IN ('solved'::zendesk_status_category, 'closed'::zendesk_status_category)
		             THEN $9::TIMESTAMPTZ ELSE NULL END)
		ON CONFLICT (org_id, zendesk_ticket_id) DO UPDATE SET
			title              = EXCLUDED.title,
			description        = EXCLUDED.description,
			reporter_id        = EXCLUDED.reporter_id,
			assignee_id        = EXCLUDED.assignee_id,
			zendesk_status     = EXCLUDED.zendesk_status,
			zendesk_updated_at = EXCLUDED.zendesk_updated_at,
			-- Preserve original resolved_at when already set; clear it if no longer solved/closed.
			resolved_at        = CASE
				WHEN EXCLUDED.zendesk_status IN ('solved'::zendesk_status_category, 'closed'::zendesk_status_category)
				THEN COALESCE(tickets.resolved_at, EXCLUDED.zendesk_updated_at)
				ELSE NULL
			END
		RETURNING id`,
		d.Subject, d.Description, reporterID, assigneeID, orgID,
		newStatus, d.ID, d.CreatedAt, d.UpdatedAt,
	).Scan(&ticketID)
	if err != nil {
		return fmt.Errorf("upsert ticket: %w", err)
	}

	// Sync default kanban only when the ticket is new or its status changed.
	isNew := oldStatus == nil
	statusChanged := oldStatus != nil && *oldStatus != newStatus
	if isNew || statusChanged {
		if err := syncTicketToDefaultKanban(ctx, tx, orgID, ticketID, newStatus); err != nil {
			return fmt.Errorf("sync kanban: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Sync all comments for this ticket from the Zendesk API. This covers the
	// case where comment.created webhooks are not fired (e.g. Zendesk does not
	// send a separate comment.created event for the initial message when a
	// ticket is first created). The insert is idempotent, so re-syncing on
	// ticket.updated is safe.
	if err := syncTicketComments(ctx, db, orgID, d.ID, limiter); err != nil {
		return fmt.Errorf("sync comments for ticket %d: %w", d.ID, err)
	}

	return nil
}

func handleTicketDeleted(ctx context.Context, db *sql.DB, orgID string, zendeskTicketID flexInt64) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM tickets WHERE org_id = $1 AND zendesk_ticket_id = $2`,
		orgID, zendeskTicketID,
	)
	return err
}

func handleTicketStatusChanged(ctx context.Context, db *sql.DB, orgID string, d *webhookTicketDetail) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	newStatus := mapZendeskStatus(d.Status)

	// Capture old status before updating so we can detect changes for kanban sync.
	var ticketID, oldStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT id, zendesk_status::text FROM tickets WHERE org_id = $1 AND zendesk_ticket_id = $2`,
		orgID, d.ID,
	).Scan(&ticketID, &oldStatus)
	if err == sql.ErrNoRows {
		// Ticket not yet in our DB; nothing to update.
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup ticket: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE tickets SET
			zendesk_status     = $1::zendesk_status_category,
			zendesk_updated_at = $2,
			resolved_at        = CASE
				WHEN $1::zendesk_status_category IN ('solved'::zendesk_status_category, 'closed'::zendesk_status_category)
				THEN COALESCE(resolved_at, $2::TIMESTAMPTZ)
				ELSE NULL
			END
		WHERE id = $3`,
		newStatus, d.UpdatedAt, ticketID,
	)
	if err != nil {
		return fmt.Errorf("update ticket status: %w", err)
	}

	if oldStatus != newStatus {
		if err := syncTicketToDefaultKanban(ctx, tx, orgID, ticketID, newStatus); err != nil {
			return fmt.Errorf("sync kanban: %w", err)
		}
	}

	return tx.Commit()
}

func handleTicketCommentAdded(ctx context.Context, db *sql.DB, orgID string, zendeskTicketID flexInt64, limiter *ratelimit.Limiter) error {
	// Ensure the ticket exists; fetch from Zendesk if we haven't seen it yet.
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM tickets WHERE org_id = $1 AND zendesk_ticket_id = $2)`,
		orgID, zendeskTicketID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("look up ticket: %w", err)
	}
	if !exists {
		ticket, fetchErr := fetchZendeskTicket(ctx, db, orgID, zendeskTicketID, limiter)
		if fetchErr != nil {
			return fmt.Errorf("fetch ticket %d: %w", zendeskTicketID, fetchErr)
		}
		if ticket == nil {
			return fmt.Errorf("ticket %d not found and Zendesk credentials not configured", zendeskTicketID)
		}
		if upsertErr := handleTicketUpsert(ctx, db, orgID, ticket, limiter); upsertErr != nil {
			return fmt.Errorf("upsert fetched ticket %d: %w", zendeskTicketID, upsertErr)
		}
	}
	return syncTicketComments(ctx, db, orgID, zendeskTicketID, limiter)
}

// ── Web chat parsing ──────────────────────────────────────────────────────────

type webChatLine struct {
	Speaker string
	Role    string // "customer" or "agent"
	Time    string
	Text    string
}

var (
	// Matches individual web chat transcript lines: (HH:MM:SS) Speaker: text
	reChatLine = regexp.MustCompile(`^\((\d{1,2}:\d{2}:\d{2})\)\s+(.+?):\s*(.*)$`)
)

// parseWebChatBody splits a web chat transcript body into individual lines.
// Lines start with "(HH:MM:SS) Speaker: text"; continuation lines (no timestamp)
// are accumulated into the preceding message's text.
func parseWebChatBody(body string) []webChatLine {
	var lines []webChatLine
	var current *webChatLine

	for _, rawLine := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		m := reChatLine.FindStringSubmatch(trimmed)
		if m != nil {
			// Save the previous message before starting a new one.
			if current != nil {
				lines = append(lines, *current)
			}
			speaker := strings.TrimSpace(m[2])
			lower := strings.ToLower(speaker)
			role := "agent"
			if strings.HasPrefix(lower, "web user") {
				role = "customer"
			}
			current = &webChatLine{
				Speaker: speaker,
				Role:    role,
				Time:    m[1],
				Text:    strings.TrimSpace(m[3]),
			}
		} else if current != nil && trimmed != "" {
			// Non-empty continuation line — append to the current message.
			current.Text += "\n" + trimmed
		}
	}

	// Flush the final message.
	if current != nil && current.Text != "" {
		lines = append(lines, *current)
	}

	return lines
}

// insertCommentForTicket resolves the comment author and inserts the appropriate
// ticket_comments row(s) for d within tx. Web-chat bodies are split into one row
// per transcript line; all other comments produce a single row. Also marks the
// ticket as AI-summary-stale.
func insertCommentForTicket(ctx context.Context, db *sql.DB, tx *sql.Tx, orgID, ticketID string, d *webhookCommentDetail, limiter *ratelimit.Limiter) error {
	var customerAuthorID *string
	var agentAuthorID *string
	var role string

	if d.AuthorID <= 0 {
		// author_id <= 0 is the Zendesk system/automation user.
		id, err := resolveSystemAgent(ctx, db, orgID)
		if err != nil {
			return fmt.Errorf("resolve system agent: %w", err)
		}
		agentAuthorID = &id
		role = "agent"
	} else {
		var customerID string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM customers WHERE org_id = $1 AND zendesk_user_id = $2`,
			orgID, d.AuthorID,
		).Scan(&customerID); err == nil {
			customerAuthorID = &customerID
			role = "customer"
		} else {
			var agentID string
			if err := tx.QueryRowContext(ctx,
				`SELECT id FROM agents WHERE org_id = $1 AND zendesk_user_id = $2`,
				orgID, d.AuthorID,
			).Scan(&agentID); err == nil {
				agentAuthorID = &agentID
				role = "agent"
			} else {
				fetched, fetchErr := fetchZendeskUser(ctx, db, orgID, d.AuthorID, limiter)
				if fetchErr != nil || fetched == nil {
					return fmt.Errorf("author %d not found (may arrive in a later event)", d.AuthorID)
				}
				if fetched.Role == "end-user" {
					cid, upsertErr := upsertCustomer(ctx, tx, orgID, fetched.ID, fetched.Name, fetched.Email)
					if upsertErr != nil {
						return fmt.Errorf("upsert author customer: %w", upsertErr)
					}
					customerAuthorID = &cid
					role = "customer"
				} else {
					aid, upsertErr := upsertAgent(ctx, tx, orgID, fetched.ID, fetched.Name, fetched.Email)
					if upsertErr != nil {
						return fmt.Errorf("upsert author agent: %w", upsertErr)
					}
					agentAuthorID = &aid
					role = "agent"
				}
			}
		}
	}

	// Zendesk uses via.channel="chat_transcript" for all web chat messages.
	if d.Via.Channel == "chat_transcript" {
		chatLines := parseWebChatBody(d.Body)
		systemAgentID, err := resolveSystemAgent(ctx, db, orgID)
		if err != nil {
			return fmt.Errorf("resolve system agent: %w", err)
		}

		// Customer-role lines are attributed to the ticket's reporter.
		// All chat_transcript comments come from author_id=-1 so customerAuthorID
		// is always nil; we resolve the reporter directly from the tickets row.
		var chatCustomerID *string
		if customerAuthorID != nil {
			chatCustomerID = customerAuthorID
		} else {
			var rid string
			if err := tx.QueryRowContext(ctx,
				`SELECT reporter_id FROM tickets WHERE id = $1`, ticketID,
			).Scan(&rid); err == nil {
				chatCustomerID = &rid
			}
		}

		for i, line := range chatLines {
			var lineCustomerAuthorID *string
			var lineAgentAuthorID *string
			if line.Role == "customer" {
				lineCustomerAuthorID = chatCustomerID
			} else {
				lineAgentAuthorID = &systemAgentID
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ticket_comments
					(ticket_id, customer_author_id, agent_author_id, role, body, html_body,
					 channel, zendesk_comment_id, zendesk_sub_index, author_display_name,
					 received_at)
				VALUES ($1, $2, $3, $4::comment_role, $5, NULL,
				        'chat'::comment_channel, $6, $7, $8,
				        $9)
				ON CONFLICT (ticket_id, zendesk_comment_id, zendesk_sub_index) DO NOTHING`,
				ticketID, lineCustomerAuthorID, lineAgentAuthorID, line.Role,
				line.Text, d.ID, i, line.Speaker, d.CreatedAt,
			); err != nil {
				return fmt.Errorf("insert web chat line %d: %w", i, err)
			}
		}
	} else {
		// Regular comment — extract any voice data and insert a single row.
		var callID *int64
		var recordingURL, transcriptionText, transcriptionStatus *string
		var callDuration *int
		var callFrom, callTo, answeredByName, callLocation *string
		var callStartedAt *time.Time
		if d.Data != nil {
			callID = d.Data.CallID
			recordingURL = d.Data.RecordingURL
			transcriptionText = d.Data.TranscriptionText
			transcriptionStatus = d.Data.TranscriptionStatus
			callDuration = d.Data.CallDuration
			callFrom = d.Data.callFrom()
			callTo = d.Data.callTo()
			callLocation = d.Data.Location
			callStartedAt = d.Data.StartedAt
			answeredByName = d.Data.AnsweredByName
			if answeredByName == nil && d.Data.AnsweredByID != nil {
				var name string
				if err := tx.QueryRowContext(ctx,
					`SELECT name FROM agents WHERE org_id = $1 AND zendesk_user_id = $2`,
					orgID, *d.Data.AnsweredByID,
				).Scan(&name); err == nil {
					answeredByName = &name
				}
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ticket_comments
				(ticket_id, customer_author_id, agent_author_id, role, body, html_body, channel, zendesk_comment_id, zendesk_sub_index, received_at,
				 call_id, recording_url, transcription_text, transcription_status, call_duration,
				 call_from, call_to, answered_by_name, call_location, call_started_at)
			VALUES ($1, $2, $3, $4::comment_role, $5, $6, $7::comment_channel, $8, 0, $9,
			        $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
			ON CONFLICT (ticket_id, zendesk_comment_id, zendesk_sub_index) DO NOTHING`,
			ticketID, customerAuthorID, agentAuthorID, role,
			d.Body, nilIfEmpty(d.HtmlBody), mapCommentChannel(d.Via.Channel, d.Public), d.ID, d.CreatedAt,
			callID, recordingURL, transcriptionText, transcriptionStatus, callDuration,
			callFrom, callTo, answeredByName, callLocation, callStartedAt,
		); err != nil {
			return fmt.Errorf("upsert comment: %w", err)
		}
	}

	// Reset error count so tickets with new comments get a fresh AI summary attempt.
	if _, err := tx.ExecContext(ctx,
		`UPDATE tickets SET ai_summary_stale = TRUE, ai_summary_error_count = 0 WHERE id = $1`, ticketID,
	); err != nil {
		return fmt.Errorf("mark ticket stale: %w", err)
	}

	return nil
}

func handleCommentCreated(ctx context.Context, db *sql.DB, orgID string, d *webhookCommentDetail, limiter *ratelimit.Limiter) error {
	// Resolve the ticket, fetching from Zendesk if not yet in the DB.
	var ticketID string
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM tickets WHERE org_id = $1 AND zendesk_ticket_id = $2`,
		orgID, d.TicketID,
	).Scan(&ticketID); err == sql.ErrNoRows {
		ticket, fetchErr := fetchZendeskTicket(ctx, db, orgID, d.TicketID, limiter)
		if fetchErr != nil {
			return fmt.Errorf("fetch ticket %d: %w", d.TicketID, fetchErr)
		}
		if ticket == nil {
			return fmt.Errorf("ticket %d not found and Zendesk credentials not configured", d.TicketID)
		}
		if upsertErr := handleTicketUpsert(ctx, db, orgID, ticket, limiter); upsertErr != nil {
			return fmt.Errorf("upsert fetched ticket %d: %w", d.TicketID, upsertErr)
		}
		if err := db.QueryRowContext(ctx,
			`SELECT id FROM tickets WHERE org_id = $1 AND zendesk_ticket_id = $2`,
			orgID, d.TicketID,
		).Scan(&ticketID); err != nil {
			return fmt.Errorf("look up upserted ticket %d: %w", d.TicketID, err)
		}
	} else if err != nil {
		return fmt.Errorf("look up ticket: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := insertCommentForTicket(ctx, db, tx, orgID, ticketID, d, limiter); err != nil {
		return err
	}

	return tx.Commit()
}

func handleCommentUpdated(ctx context.Context, db *sql.DB, orgID string, d *webhookCommentDetail, limiter *ratelimit.Limiter) error {
	// Resolve the ticket. If it doesn't exist yet, there's nothing to update.
	var ticketID string
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM tickets WHERE org_id = $1 AND zendesk_ticket_id = $2`,
		orgID, d.TicketID,
	).Scan(&ticketID); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return fmt.Errorf("look up ticket: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Delete all existing rows for this Zendesk comment (may be multiple for web chat).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ticket_comments WHERE ticket_id = $1 AND zendesk_comment_id = $2`,
		ticketID, d.ID,
	); err != nil {
		return fmt.Errorf("delete comment rows: %w", err)
	}

	if err := insertCommentForTicket(ctx, db, tx, orgID, ticketID, d, limiter); err != nil {
		return err
	}

	return tx.Commit()
}

func handleUserUpsert(ctx context.Context, db *sql.DB, orgID string, d *webhookUserDetail) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if d.Role == "end-user" {
		if _, err := upsertCustomer(ctx, tx, orgID, d.ID, d.Name, d.Email); err != nil {
			return fmt.Errorf("upsert customer: %w", err)
		}
	} else {
		// agent or admin
		if _, err := upsertAgent(ctx, tx, orgID, d.ID, d.Name, d.Email); err != nil {
			return fmt.Errorf("upsert agent: %w", err)
		}
	}

	return tx.Commit()
}

// ── Shared helpers ────────────────────────────────────────────────────────────

// resolveOrFetchCustomer returns the purl customer ID for a given Zendesk user ID.
// If not found in the DB, it fetches the user from the Zendesk REST API and upserts.
// Returns ("", nil) if the user exists in Zendesk but is not an end-user.
func resolveOrFetchCustomer(ctx context.Context, db *sql.DB, tx *sql.Tx, orgID string, zendeskUserID flexInt64, limiter *ratelimit.Limiter) (string, error) {
	var customerID string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM customers WHERE org_id = $1 AND zendesk_user_id = $2`,
		orgID, zendeskUserID,
	).Scan(&customerID)
	if err == nil {
		return customerID, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("look up customer: %w", err)
	}

	user, err := fetchZendeskUser(ctx, db, orgID, zendeskUserID, limiter)
	if err != nil {
		return "", fmt.Errorf("fetch zendesk user %d: %w", zendeskUserID, err)
	}
	if user == nil || user.Role != "end-user" {
		return "", nil
	}

	id, err := upsertCustomer(ctx, tx, orgID, user.ID, user.Name, user.Email)
	if err != nil {
		return "", fmt.Errorf("upsert fetched customer: %w", err)
	}
	return id, nil
}

// upsertCustomer inserts or updates a customer row identified by (org_id, zendesk_user_id).
// Also adds the email to customer_emails if not already present.
func upsertCustomer(ctx context.Context, tx *sql.Tx, orgID string, zendeskUserID flexInt64, name, email string) (string, error) {
	var customerID string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO customers (name, org_id, zendesk_user_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (org_id, zendesk_user_id) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`,
		name, orgID, zendeskUserID,
	).Scan(&customerID)
	if err != nil {
		return "", err
	}

	if email != "" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO customer_emails (customer_id, email, verified)
			SELECT $1, $2, false
			WHERE NOT EXISTS (
				SELECT 1 FROM customer_emails WHERE customer_id = $1 AND email = $2
			)`,
			customerID, email,
		)
		if err != nil {
			return "", fmt.Errorf("upsert email: %w", err)
		}
	}

	return customerID, nil
}

// resolveSystemAgent upserts a "Zendesk Automation" agent for the org and returns
// its purl ID. Zendesk uses author_id <= 0 for comments from triggers, automations,
// bots, and other system processes that have no real user behind them.
func resolveSystemAgent(ctx context.Context, db *sql.DB, orgID string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `
		INSERT INTO agents (email, name, org_id, zendesk_user_id)
		VALUES ('zendesk-automation@system.invalid', 'Zendesk Automation', $1, -1)
		ON CONFLICT (org_id, zendesk_user_id) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`,
		orgID,
	).Scan(&id)
	return id, err
}

// upsertAgent inserts or updates an agent row identified by (org_id, zendesk_user_id).
func upsertAgent(ctx context.Context, tx *sql.Tx, orgID string, zendeskUserID flexInt64, name, email string) (string, error) {
	var agentID string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO agents (email, name, org_id, zendesk_user_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id, zendesk_user_id) DO UPDATE SET name = EXCLUDED.name, email = EXCLUDED.email
		RETURNING id`,
		email, name, orgID, zendeskUserID,
	).Scan(&agentID)
	return agentID, err
}

// syncTicketToDefaultKanban removes a ticket from its current column in the
// default board and re-inserts it into the column matching zendesk_status.
// If no matching column exists, the ticket is simply removed from the board.
func syncTicketToDefaultKanban(ctx context.Context, tx *sql.Tx, orgID, ticketID, status string) error {
	var defaultBoardID string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM boards WHERE org_id = $1 AND is_default = true`,
		orgID,
	).Scan(&defaultBoardID)
	if err == sql.ErrNoRows {
		return nil // no default board configured
	}
	if err != nil {
		return fmt.Errorf("query default board: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM board_tickets WHERE board_id = $1 AND ticket_id = $2`,
		defaultBoardID, ticketID,
	); err != nil {
		return fmt.Errorf("remove from board: %w", err)
	}

	var columnID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM board_columns WHERE board_id = $1 AND zendesk_status = $2::zendesk_status_category`,
		defaultBoardID, status,
	).Scan(&columnID)
	if err == sql.ErrNoRows {
		return nil // status not covered by any column; ticket stays off the board
	}
	if err != nil {
		return fmt.Errorf("query column for status %q: %w", status, err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO board_tickets (board_id, column_id, ticket_id, position)
		VALUES ($1, $2, $3,
			(SELECT COALESCE(MAX(position) + 1, 0) FROM board_tickets WHERE column_id = $2))`,
		defaultBoardID, columnID, ticketID,
	)
	if err != nil {
		return fmt.Errorf("insert into board: %w", err)
	}

	return nil
}

// fetchZendeskUser fetches a single user from the Zendesk REST API using the
// org's stored credentials. Returns nil if credentials are not configured.
func fetchZendeskUser(ctx context.Context, db *sql.DB, orgID string, zendeskUserID flexInt64, limiter *ratelimit.Limiter) (*webhookUserDetail, error) {
	var subdomain, email, apiKey string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(zendesk_subdomain,''), COALESCE(zendesk_email,''), COALESCE(zendesk_api_key,'')
		 FROM organizations WHERE id = $1`,
		orgID,
	).Scan(&subdomain, &email, &apiKey)
	if err != nil {
		return nil, fmt.Errorf("load zendesk creds: %w", err)
	}
	if subdomain == "" || email == "" || apiKey == "" {
		return nil, nil
	}

	creds := base64.StdEncoding.EncodeToString([]byte(email + "/token:" + apiKey))
	if limiter != nil {
		if err := limiter.Wait(ctx, creds); err != nil {
			return nil, err
		}
	}
	url := fmt.Sprintf("https://%s.zendesk.com/api/v2/users/%d.json", subdomain, zendeskUserID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+creds)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zendesk request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zendesk returned %s: %s", resp.Status, respBody)
	}

	var wrapper struct {
		User webhookUserDetail `json:"user"`
	}
	if err := json.Unmarshal(respBody, &wrapper); err != nil {
		return nil, fmt.Errorf("parse user: %w", err)
	}
	return &wrapper.User, nil
}

// fetchZendeskTicket fetches a single ticket from the Zendesk REST API using
// the org's stored credentials. Returns nil if credentials are not configured.
func fetchZendeskTicket(ctx context.Context, db *sql.DB, orgID string, zendeskTicketID flexInt64, limiter *ratelimit.Limiter) (*webhookTicketDetail, error) {
	var subdomain, email, apiKey string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(zendesk_subdomain,''), COALESCE(zendesk_email,''), COALESCE(zendesk_api_key,'')
		 FROM organizations WHERE id = $1`,
		orgID,
	).Scan(&subdomain, &email, &apiKey)
	if err != nil {
		return nil, fmt.Errorf("load zendesk creds: %w", err)
	}
	if subdomain == "" || email == "" || apiKey == "" {
		return nil, nil
	}

	creds := base64.StdEncoding.EncodeToString([]byte(email + "/token:" + apiKey))
	if limiter != nil {
		if err := limiter.Wait(ctx, creds); err != nil {
			return nil, err
		}
	}
	url := fmt.Sprintf("https://%s.zendesk.com/api/v2/tickets/%d.json", subdomain, zendeskTicketID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+creds)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zendesk request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zendesk returned %s: %s", resp.Status, respBody)
	}

	var wrapper struct {
		Ticket webhookTicketDetail `json:"ticket"`
	}
	if err := json.Unmarshal(respBody, &wrapper); err != nil {
		return nil, fmt.Errorf("parse ticket: %w", err)
	}
	return &wrapper.Ticket, nil
}

// fetchZendeskTicketComments fetches all comments for a ticket from the Zendesk
// REST API using the org's stored credentials. Returns nil if credentials are
// not configured.
func fetchZendeskTicketComments(ctx context.Context, db *sql.DB, orgID string, zendeskTicketID flexInt64, limiter *ratelimit.Limiter) ([]webhookCommentDetail, error) {
	var subdomain, email, apiKey string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(zendesk_subdomain,''), COALESCE(zendesk_email,''), COALESCE(zendesk_api_key,'')
		 FROM organizations WHERE id = $1`,
		orgID,
	).Scan(&subdomain, &email, &apiKey)
	if err != nil {
		return nil, fmt.Errorf("load zendesk creds: %w", err)
	}
	if subdomain == "" || email == "" || apiKey == "" {
		return nil, nil
	}

	creds := base64.StdEncoding.EncodeToString([]byte(email + "/token:" + apiKey))
	if limiter != nil {
		if err := limiter.Wait(ctx, creds); err != nil {
			return nil, err
		}
	}
	url := fmt.Sprintf("https://%s.zendesk.com/api/v2/tickets/%d/comments.json", subdomain, zendeskTicketID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+creds)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zendesk request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zendesk returned %s: %s", resp.Status, respBody)
	}

	var wrapper struct {
		Comments []struct {
			ID        flexInt64         `json:"id"`
			AuthorID  flexInt64         `json:"author_id"`
			Body      string            `json:"body"`
			Public    bool              `json:"public"`
			Via       webhookCommentVia `json:"via"`
			CreatedAt time.Time         `json:"created_at"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(respBody, &wrapper); err != nil {
		return nil, fmt.Errorf("parse comments: %w", err)
	}

	comments := make([]webhookCommentDetail, len(wrapper.Comments))
	for i, c := range wrapper.Comments {
		comments[i] = webhookCommentDetail{
			ID:        c.ID,
			TicketID:  zendeskTicketID,
			AuthorID:  c.AuthorID,
			Body:      c.Body,
			Public:    c.Public,
			Via:       c.Via,
			CreatedAt: c.CreatedAt,
		}
	}
	return comments, nil
}

// syncTicketComments fetches all comments for a ticket from the Zendesk API
// and upserts each one. Returns nil if credentials are not configured.
func syncTicketComments(ctx context.Context, db *sql.DB, orgID string, zendeskTicketID flexInt64, limiter *ratelimit.Limiter) error {
	comments, err := fetchZendeskTicketComments(ctx, db, orgID, zendeskTicketID, limiter)
	if err != nil {
		return fmt.Errorf("fetch comments: %w", err)
	}
	if comments == nil {
		return nil // credentials not configured
	}
	for _, c := range comments {
		if err := handleCommentCreated(ctx, db, orgID, &c, limiter); err != nil {
			return fmt.Errorf("upsert comment %d: %w", c.ID, err)
		}
	}
	return nil
}

// mapZendeskStatus maps a raw Zendesk ticket status string to our
// zendesk_status_category enum value. Accepts mixed case (e.g. "SOLVED").
func mapZendeskStatus(s string) string {
	switch strings.ToLower(s) {
	case "new", "open", "pending", "solved", "closed":
		return strings.ToLower(s)
	case "hold":
		return "pending"
	default:
		return "open"
	}
}

// mapCommentChannel maps a Zendesk via.channel value and public flag to our
// comment_channel enum. Private comments are always "internal".
func mapCommentChannel(viaChannel string, public bool) string {
	if !public {
		return "internal"
	}
	switch viaChannel {
	case "email":
		return "email"
	case "sms", "native_messaging", "whatsapp":
		return "sms"
	case "voice", "phone":
		return "voice"
	default:
		return "web"
	}
}
