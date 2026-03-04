package app

import (
	"context"
	"database/sql"
	"log"
	"sync"

	"purl/api/internal/ratelimit"
)

// CatchUpZendeskTickets fetches the latest state of every ticket in our DB
// from the Zendesk API and upserts it (ticket fields + all comments). Useful
// after deploying new webhook handlers to backfill missed events.
//
// Tickets are processed with 3 concurrent workers. Failures are logged and
// skipped so one bad ticket doesn't abort the whole run.
func CatchUpZendeskTickets(ctx context.Context, db *sql.DB, limiter *ratelimit.Limiter, orgID string) error {
	rows, err := db.QueryContext(ctx,
		`SELECT zendesk_ticket_id FROM tickets WHERE org_id = $1 AND zendesk_ticket_id IS NOT NULL ORDER BY zendesk_ticket_id`,
		orgID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []flexInt64
	for rows.Next() {
		var id flexInt64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	total := len(ids)
	log.Printf("catch-up: %d tickets to sync", total)

	work := make(chan flexInt64, total)
	for _, id := range ids {
		work <- id
	}
	close(work)

	var (
		mu       sync.Mutex
		done     int
		failures int
	)

	const workers = 3
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for zendeskID := range work {
				if err := catchUpTicket(ctx, db, limiter, orgID, zendeskID); err != nil {
					log.Printf("catch-up: ticket %d failed: %v", int64(zendeskID), err)
					mu.Lock()
					failures++
					done++
					n := done
					mu.Unlock()
					log.Printf("catch-up: %d/%d done (%d failures)", n, total, failures)
					continue
				}
				mu.Lock()
				done++
				n := done
				f := failures
				mu.Unlock()
				log.Printf("catch-up: %d/%d done (%d failures)", n, total, f)
			}
		}()
	}
	wg.Wait()

	log.Printf("catch-up: complete — %d succeeded, %d failed", total-failures, failures)
	return nil
}

func catchUpTicket(ctx context.Context, db *sql.DB, limiter *ratelimit.Limiter, orgID string, zendeskID flexInt64) error {
	ticket, err := fetchZendeskTicket(ctx, db, orgID, zendeskID, limiter)
	if err != nil {
		return err
	}
	if ticket == nil {
		return nil // no credentials configured; skip silently
	}
	return handleTicketUpsert(ctx, db, orgID, ticket, limiter)
}
