// Package pgstore is the Postgres implementation of store.Store, using pgx.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arielagor/floorrules-go/internal/domain"
	"github.com/arielagor/floorrules-go/internal/id"
	"github.com/arielagor/floorrules-go/internal/store"
)

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// New wraps an existing pool. The caller owns the pool's lifecycle.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Ping implements store.Store; /readyz uses it.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const uniqueViolation = "23505"

func constraintViolated(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == name
}

func insertAudit(ctx context.Context, tx pgx.Tx, actor, pub, action, entity string, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (actor, publisher_id, action, entity_id, detail)
		VALUES ($1, $2, $3, $4, $5)`, actor, pub, action, entity, raw)
	return err
}

func insertOutbox(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox (id, topic, payload) VALUES ($1, $2, $3)`, id.New(), topic, raw)
	return err
}

const ruleColumns = `id::text, publisher_id, device, geo, genre, demand_partner, floor_micros,
	currency, status, version, created_by, created_at, updated_at`

func scanRule(row pgx.Row) (domain.Rule, error) {
	var r domain.Rule
	var status string
	err := row.Scan(&r.ID, &r.PublisherID, &r.Segment.Device, &r.Segment.Geo, &r.Segment.Genre,
		&r.Segment.DemandPartner, &r.FloorMicros, &r.Currency, &status, &r.Version, &r.CreatedBy,
		&r.CreatedAt, &r.UpdatedAt)
	r.Status = domain.RuleStatus(status)
	return r, err
}

// CreateRule implements store.Store.
func (s *Store) CreateRule(ctx context.Context, r domain.Rule) (domain.Rule, error) {
	var out domain.Rule
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanRule(tx.QueryRow(ctx, `INSERT INTO rules
			(id, publisher_id, device, geo, genre, demand_partner, floor_micros, currency, status, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', $9)
			RETURNING `+ruleColumns,
			r.ID, r.PublisherID, r.Segment.Device, r.Segment.Geo, r.Segment.Genre, r.Segment.DemandPartner,
			r.FloorMicros, r.Currency, r.CreatedBy))
		if err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, r.CreatedBy, r.PublisherID, "rule.create", out.ID, out); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, domain.TopicRuleChanged,
			domain.RuleChangedPayload{PublisherID: out.PublisherID, RuleID: out.ID, Change: "created"})
	})
	if constraintViolated(err, "rules_one_active_per_segment") {
		return domain.Rule{}, store.ErrConflict
	}
	return out, err
}

// DisableRule implements store.Store.
func (s *Store) DisableRule(ctx context.Context, publisherID, ruleID, actor string) (domain.Rule, error) {
	var out domain.Rule
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanRule(tx.QueryRow(ctx, `UPDATE rules
			SET status = 'disabled', version = version + 1, updated_at = now()
			WHERE id = $1 AND publisher_id = $2 AND status = 'active'
			RETURNING `+ruleColumns, ruleID, publisherID))
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, actor, publisherID, "rule.disable", ruleID, map[string]any{"version": out.Version}); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, domain.TopicRuleChanged,
			domain.RuleChangedPayload{PublisherID: publisherID, RuleID: ruleID, Change: "disabled"})
	})
	return out, err
}

// ListRules implements store.Store.
func (s *Store) ListRules(ctx context.Context, publisherID string) ([]domain.Rule, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ruleColumns+` FROM rules
		WHERE publisher_id = $1 ORDER BY seq`, publisherID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func insertPlan(ctx context.Context, tx pgx.Tx, p domain.Plan) error {
	if p.Ops == nil {
		p.Ops = []domain.Op{}
	}
	ops, err := json.Marshal(p.Ops)
	if err != nil {
		return err
	}
	var createdAt *time.Time
	if !p.CreatedAt.IsZero() {
		createdAt = &p.CreatedAt
	}
	if _, err := tx.Exec(ctx, `INSERT INTO plans (id, publisher_id, ops, base_fingerprint, status, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()))`,
		p.ID, p.PublisherID, ops, p.BaseFingerprint, string(p.Status), p.CreatedBy, createdAt); err != nil {
		return err
	}
	return insertAudit(ctx, tx, p.CreatedBy, p.PublisherID, "plan.create", p.ID, map[string]any{"ops": len(p.Ops)})
}

// CreatePlan implements store.Store.
func (s *Store) CreatePlan(ctx context.Context, p domain.Plan) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return insertPlan(ctx, tx, p) })
}

// GetPlan implements store.Store.
func (s *Store) GetPlan(ctx context.Context, publisherID, planID string) (domain.Plan, error) {
	var p domain.Plan
	var ops []byte
	var status string
	err := s.pool.QueryRow(ctx, `SELECT id::text, publisher_id, ops, base_fingerprint, status, created_by, created_at
		FROM plans WHERE id = $1 AND publisher_id = $2`, planID, publisherID).
		Scan(&p.ID, &p.PublisherID, &ops, &p.BaseFingerprint, &status, &p.CreatedBy, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Plan{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Plan{}, err
	}
	p.Status = domain.PlanStatus(status)
	if err := json.Unmarshal(ops, &p.Ops); err != nil {
		return domain.Plan{}, fmt.Errorf("decode plan ops: %w", err)
	}
	return p, nil
}

const attemptColumns = `idempotency_key, plan_id::text, request_hash, status, actor, result, started_at, finished_at,
	attempt_token::text`

func scanAttempt(row pgx.Row) (domain.ApplyAttempt, error) {
	var a domain.ApplyAttempt
	var status string
	var result []byte
	err := row.Scan(&a.IdempotencyKey, &a.PlanID, &a.RequestHash, &status, &a.Actor, &result, &a.StartedAt, &a.FinishedAt,
		&a.Token)
	if err != nil {
		return a, err
	}
	a.Status = domain.AttemptStatus(status)
	if len(result) > 0 {
		a.Result = &domain.ApplyResult{}
		if err := json.Unmarshal(result, a.Result); err != nil {
			return a, fmt.Errorf("decode apply result: %w", err)
		}
	}
	return a, nil
}

// GetAttempt implements store.Store.
func (s *Store) GetAttempt(ctx context.Context, key string) (domain.ApplyAttempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx, `SELECT `+attemptColumns+`
		FROM apply_attempts WHERE idempotency_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ApplyAttempt{}, store.ErrNotFound
	}
	return a, err
}

// BeginApply implements store.Store. The lease is compared with the
// database's now(), the same clock that wrote started_at.
func (s *Store) BeginApply(ctx context.Context, a domain.ApplyAttempt, lease time.Duration) (domain.ApplyAttempt, bool, error) {
	var out domain.ApplyAttempt
	var created bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Every claim on a plan serialises on the plan's row lock, so the
		// pending check and the insert below are one step. Checked outside
		// this transaction, a second key could pass the check, wait while
		// the first key finished, and then claim an applied plan.
		var planStatus string
		err := tx.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1 FOR UPDATE`, a.PlanID).Scan(&planStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}

		selectKey := `SELECT ` + attemptColumns + ` FROM apply_attempts WHERE idempotency_key = $1 FOR UPDATE`
		out, err = scanAttempt(tx.QueryRow(ctx, selectKey, a.IdempotencyKey))
		if errors.Is(err, pgx.ErrNoRows) {
			// A new key: claim it only while the plan is pending.
			if domain.PlanStatus(planStatus) != domain.PlanPending {
				return store.ErrPlanNotPending
			}
			out, err = scanAttempt(tx.QueryRow(ctx, `INSERT INTO apply_attempts
				(idempotency_key, plan_id, request_hash, status, actor, attempt_token)
				VALUES ($1, $2, $3, 'in_progress', $4, gen_random_uuid())
				ON CONFLICT (idempotency_key) DO NOTHING
				RETURNING `+attemptColumns, a.IdempotencyKey, a.PlanID, a.RequestHash, a.Actor))
			if errors.Is(err, pgx.ErrNoRows) {
				// A concurrent claim on another plan took the key first;
				// answer from its record.
				out, err = scanAttempt(tx.QueryRow(ctx, selectKey, a.IdempotencyKey))
				return err
			}
			created = err == nil
			return err
		}
		if err != nil {
			return err
		}
		// The key exists: replay, or reclaim an abandoned attempt.
		if out.Status != domain.AttemptInProgress || out.RequestHash != a.RequestHash {
			return nil
		}
		// Reclaim only if the lease has expired. A new token fences off the
		// previous holder, should it still be running.
		reclaimed, err := scanAttempt(tx.QueryRow(ctx, `UPDATE apply_attempts
			SET started_at = now(), actor = $2, attempt_token = gen_random_uuid()
			WHERE idempotency_key = $1 AND started_at < now() - ($3::bigint * interval '1 millisecond')
			RETURNING `+attemptColumns, a.IdempotencyKey, a.Actor, lease.Milliseconds()))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // lease still live: the holder is presumed to be running
		}
		if err != nil {
			return err
		}
		out, created = reclaimed, true
		return nil
	})
	if constraintViolated(err, "apply_attempts_one_inflight_per_plan") {
		return domain.ApplyAttempt{}, false, store.ErrPlanBusy
	}
	if err != nil {
		return domain.ApplyAttempt{}, false, err
	}
	if !created {
		out.Token = "" // only the claimant gets a usable token
	}
	return out, created, nil
}

// FinishApply implements store.Store.
func (s *Store) FinishApply(ctx context.Context, publisherID string, a domain.ApplyAttempt, result domain.ApplyResult) error {
	status := domain.AttemptFailed
	if result.Status == domain.PlanApplied {
		status = domain.AttemptSucceeded
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var planID string
		err := tx.QueryRow(ctx, `UPDATE apply_attempts SET status = $3, result = $4, finished_at = now()
			WHERE idempotency_key = $1 AND attempt_token::text = $2 AND status = 'in_progress'
			RETURNING plan_id::text`, a.IdempotencyKey, a.Token, string(status), raw).Scan(&planID)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM apply_attempts WHERE idempotency_key = $1)`,
				a.IdempotencyKey).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return store.ErrNotFound
			}
			return store.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		actor, key := a.Actor, a.IdempotencyKey
		// Only a pending plan moves; a terminal status is never overwritten.
		// The attempt, audit row and event are still written: they record
		// what this apply did on the platform.
		tag, err := tx.Exec(ctx, `UPDATE plans SET status = $3 WHERE id = $1 AND publisher_id = $2 AND status = 'pending'`,
			planID, publisherID, string(result.Status))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plans WHERE id = $1 AND publisher_id = $2)`,
				planID, publisherID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return store.ErrNotFound
			}
		}
		if err := insertAudit(ctx, tx, actor, publisherID, "plan.apply", planID,
			map[string]any{"status": result.Status, "idempotency_key": key}); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, domain.TopicPlanApplied,
			map[string]any{"publisher_id": publisherID, "plan_id": planID, "status": result.Status})
	})
}

// ListAudit implements store.Store, newest first.
func (s *Store) ListAudit(ctx context.Context, publisherID string, limit int) ([]domain.AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, at, actor, publisher_id, action, entity_id, detail
		FROM audit_log WHERE publisher_id = $1 ORDER BY id DESC LIMIT $2`, publisherID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AuditEntry{}
	for rows.Next() {
		var e domain.AuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.PublisherID, &e.Action, &e.EntityID, &detail); err != nil {
			return nil, err
		}
		e.Detail = detail
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClaimOutbox implements store.Store. SKIP LOCKED lets several relays claim
// disjoint batches without blocking on each other.
func (s *Store) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]domain.Event, error) {
	rows, err := s.pool.Query(ctx, `UPDATE outbox SET claimed_until = now() + ($2::bigint * interval '1 millisecond')
		WHERE id IN (
			SELECT id FROM outbox
			WHERE sent_at IS NULL AND (claimed_until IS NULL OR claimed_until <= now())
			ORDER BY seq LIMIT $1
			FOR UPDATE SKIP LOCKED)
		RETURNING seq, id::text, topic, payload, created_at`, limit, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type seqEvent struct {
		seq int64
		ev  domain.Event
	}
	var batch []seqEvent
	for rows.Next() {
		var se seqEvent
		var payload []byte
		if err := rows.Scan(&se.seq, &se.ev.ID, &se.ev.Topic, &payload, &se.ev.CreatedAt); err != nil {
			return nil, err
		}
		se.ev.Payload = payload
		batch = append(batch, se)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// UPDATE ... RETURNING has no guaranteed order; restore outbox order.
	sort.Slice(batch, func(i, j int) bool { return batch[i].seq < batch[j].seq })
	out := make([]domain.Event, 0, len(batch))
	for _, se := range batch {
		out = append(out, se.ev)
	}
	return out, nil
}

// MarkOutboxSent implements store.Store.
func (s *Store) MarkOutboxSent(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE outbox SET sent_at = now() WHERE id = ANY($1::uuid[])`, ids)
	return err
}

// CreatePlanForEvent implements store.Store.
func (s *Store) CreatePlanForEvent(ctx context.Context, consumer, eventID string, p domain.Plan) (bool, error) {
	var created bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO processed_events (consumer, event_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, consumer, eventID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		created = true
		return insertPlan(ctx, tx, p)
	})
	return created, err
}
