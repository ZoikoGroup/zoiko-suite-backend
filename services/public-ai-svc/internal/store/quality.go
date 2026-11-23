package store

import (
	"context"
	"sort"
	"time"
)

// QualityCase is one answer a visitor marked not useful, queued for blinded
// review and remediation (Doc 09 §19). It carries the turn and the public
// reply the visitor saw, never the session or the visitor's own words.
type QualityCase struct {
	TurnID         string
	IntentID       string
	Status         string
	FeedbackReason string
	Response       []byte // the validated public envelope
	CreatedAt      time.Time
}

// QualityQueue lists not-useful answers, newest first.
type QualityQueue interface {
	QualityCases(ctx context.Context, limit int) ([]QualityCase, error)
}

func (m *Memory) QualityCases(_ context.Context, limit int) ([]QualityCase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []QualityCase
	for _, t := range m.turns {
		if t.FeedbackRating == "not_useful" {
			out = append(out, QualityCase{TurnID: t.TurnID, IntentID: t.IntentID, Status: t.Status, FeedbackReason: t.FeedbackReason,
				Response: append([]byte(nil), t.Response...), CreatedAt: t.CreatedAt})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (p *Postgres) QualityCases(ctx context.Context, limit int) ([]QualityCase, error) {
	rows, err := p.pool.Query(ctx, `SELECT turn_id, intent_id, status, COALESCE(feedback_reason, ''),
		COALESCE(response, ''::bytea), created_at
		FROM public_ai.turns WHERE feedback_rating = 'not_useful' ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QualityCase
	for rows.Next() {
		var c QualityCase
		var resp []byte
		if err := rows.Scan(&c.TurnID, &c.IntentID, &c.Status, &c.FeedbackReason, &resp, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Response = resp
		out = append(out, c)
	}
	return out, rows.Err()
}
