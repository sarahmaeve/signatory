package pipeline_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func openTestStore(t *testing.T) *pipeline.Store {
	t.Helper()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)
	return s
}

func TestCreateAndGetSession(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/foo/bar", `{"lang":"go"}`)
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "repo:github/foo/bar", sess.Target)
	assert.Equal(t, "active", sess.Status)
	assert.False(t, sess.CreatedAt.IsZero())

	got, err := s.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.ID)
	assert.Equal(t, sess.Target, got.Target)
	assert.Equal(t, `{"lang":"go"}`, got.Metadata)
}

func TestUpdateSessionStatus(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/foo/bar", "")
	require.NoError(t, err)

	require.NoError(t, s.UpdateSessionStatus(ctx, sess.ID, "complete"))

	got, err := s.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "complete", got.Status)
}

func TestUpdateSessionStatus_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	err := s.UpdateSessionStatus(context.Background(), "nonexistent", "complete")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDepositAndGetMessages(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/foo/bar", "")
	require.NoError(t, err)

	msg1, err := s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess.ID,
		Role:      "security",
		MsgType:   "handoff",
		Content:   "# Security handoff content",
	})
	require.NoError(t, err)
	assert.NotZero(t, msg1.ID)

	msg2, err := s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess.ID,
		Role:      "provenance",
		MsgType:   "handoff",
		Content:   "# Provenance handoff content",
	})
	require.NoError(t, err)
	assert.NotZero(t, msg2.ID)

	// Get all messages for session.
	all, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	assert.Len(t, all, 2)

	// Filter by role.
	secOnly, err := s.GetMessages(ctx, pipeline.MessageFilter{
		SessionID: sess.ID,
		Role:      "security",
	})
	require.NoError(t, err)
	require.Len(t, secOnly, 1)
	assert.Equal(t, "# Security handoff content", secOnly[0].Content)

	// Filter by type.
	handoffs, err := s.GetMessages(ctx, pipeline.MessageFilter{
		SessionID: sess.ID,
		MsgType:   "handoff",
	})
	require.NoError(t, err)
	assert.Len(t, handoffs, 2)
}

func TestGetLatestMessage(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/foo/bar", "")
	require.NoError(t, err)

	_, err = s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess.ID,
		Role:      "security",
		MsgType:   "feedback",
		Content:   "first feedback",
	})
	require.NoError(t, err)

	_, err = s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess.ID,
		Role:      "security",
		MsgType:   "feedback",
		Content:   "second feedback",
	})
	require.NoError(t, err)

	latest, err := s.GetLatestMessage(ctx, pipeline.MessageFilter{
		SessionID: sess.ID,
		Role:      "security",
		MsgType:   "feedback",
	})
	require.NoError(t, err)
	assert.Equal(t, "second feedback", latest.Content)
}

func TestSessionIsolation(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess1, err := s.CreateSession(ctx, "repo:github/foo/alpha", "")
	require.NoError(t, err)
	sess2, err := s.CreateSession(ctx, "repo:github/foo/beta", "")
	require.NoError(t, err)

	_, err = s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess1.ID, Role: "security", MsgType: "handoff",
		Content: "alpha handoff",
	})
	require.NoError(t, err)

	_, err = s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess2.ID, Role: "security", MsgType: "handoff",
		Content: "beta handoff",
	})
	require.NoError(t, err)

	// Session 1 only sees its own messages.
	msgs, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess1.ID})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "alpha handoff", msgs[0].Content)

	// Session 2 only sees its own messages.
	msgs, err = s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess2.ID})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "beta handoff", msgs[0].Content)
}

func TestDeleteSession_CascadesMessages(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/foo/bar", "")
	require.NoError(t, err)

	_, err = s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess.ID, Role: "security", MsgType: "handoff",
		Content: "will be deleted",
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteSession(ctx, sess.ID))

	// Session is gone.
	_, err = s.GetSession(ctx, sess.ID)
	require.Error(t, err)

	// Messages are gone.
	msgs, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	assert.Empty(t, msgs)
}

func TestListSessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.CreateSession(ctx, "repo:github/foo/first", "")
	require.NoError(t, err)
	_, err = s.CreateSession(ctx, "repo:github/foo/second", "")
	require.NoError(t, err)

	sessions, err := s.ListSessions(ctx)
	require.NoError(t, err)
	assert.Len(t, sessions, 2)
	// Both present — ordering within same timestamp is not guaranteed.
	targets := []string{sessions[0].Target, sessions[1].Target}
	assert.Contains(t, targets, "repo:github/foo/first")
	assert.Contains(t, targets, "repo:github/foo/second")
}
