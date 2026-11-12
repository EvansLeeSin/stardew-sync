// Package api defines the wire protocol shared by server and Go clients.
package api

import (
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
)

// Revision is one stored version of a save.
type Revision struct {
	Rev          int          `json:"rev"`
	BaseRevision int          `json:"base_revision"`
	Device       string       `json:"device"`
	Hash         string       `json:"hash"`
	Size         int64        `json:"size"`
	UploadedAt   time.Time    `json:"uploaded_at"`
	RestoredFrom int          `json:"restored_from,omitempty"`
	Meta         savefmt.Meta `json:"meta"`
	GameDate     string       `json:"game_date"`
}

// SaveSummary is returned by GET /saves.
type SaveSummary struct {
	ID     string    `json:"id"`
	Latest *Revision `json:"latest,omitempty"`
	Lock   *Lock     `json:"lock,omitempty"`
}

// UploadMeta is the "meta" form field of POST /saves/{id}/revisions.
type UploadMeta struct {
	BaseRevision int  `json:"base_revision"`
	Force        bool `json:"force"`
}

// UploadResult is returned by a successful upload.
type UploadResult struct {
	Revision  Revision `json:"revision"`
	Duplicate bool     `json:"duplicate"`
}

// ConflictError is the body of 409 responses.
type ConflictError struct {
	Error  string    `json:"error"`
	Latest *Revision `json:"latest"`
}

// Lock is the play lock state.
type Lock struct {
	Holder        string    `json:"holder"`
	Since         time.Time `json:"since"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	ExpiresAt     time.Time `json:"expires_at"`
	GameDate      string    `json:"game_date,omitempty"`
}

// LockRequest is the body of PUT /saves/{id}/lock.
type LockRequest struct {
	Force    bool   `json:"force"`
	GameDate string `json:"game_date,omitempty"`
}

// LockResponse is returned by lock endpoints. On 423 Acquired=false.
type LockResponse struct {
	Acquired bool  `json:"acquired"`
	Lock     *Lock `json:"lock,omitempty"`
}

// ErrorBody is a generic error response.
type ErrorBody struct {
	Error string `json:"error"`
}

const (
	LockTTL           = 3 * time.Minute
	LockHeartbeat     = 60 * time.Second
	MaxWaitTimeout    = 55 * time.Second
	DefaultWaitTimout = 50 * time.Second
)
