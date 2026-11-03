//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type writableSettingRepoStub struct {
	*settingRepoStub
	setCalls int
	setErr   error
}

func newWritableSettingRepoStub(values map[string]string) *writableSettingRepoStub {
	if values == nil {
		values = map[string]string{}
	}
	return &writableSettingRepoStub{settingRepoStub: &settingRepoStub{values: values}}
}

func (s *writableSettingRepoStub) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCalls++
	if s.setErr != nil {
		return s.setErr
	}
	s.values[key] = value
	return nil
}

func TestNormalizeRegistrationEmailBlacklistEntry(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: "  Alice@Gmail.COM ", want: "alice@gmail.com", ok: true},
		{raw: "a.b+tag@googlemail.com", want: "a.b+tag@googlemail.com", ok: true},
		{raw: "user@example.com.", want: "user@example.com", ok: true},
		{raw: "", ok: false},
		{raw: "@gmail.com", ok: false},
		{raw: "user@", ok: false},
		{raw: "user", ok: false},
		{raw: "a@b@gmail.com", ok: false},
		{raw: "user@localhost", ok: false},
		{raw: "us er@gmail.com", ok: false},
		{raw: "user@bad_domain.com", ok: false},
		{raw: fmt.Sprintf("%065d@gmail.com", 0), ok: false},
	}
	for _, tc := range cases {
		got, ok := NormalizeRegistrationEmailBlacklistEntry(tc.raw)
		require.Equal(t, tc.ok, ok, tc.raw)
		require.Equal(t, tc.want, got, tc.raw)
	}
}

func TestNormalizeRegistrationEmailBlacklist_DedupesAndKeepsOrder(t *testing.T) {
	got, err := NormalizeRegistrationEmailBlacklist([]string{
		"B@gmail.com", " ", "a@googlemail.com", "b@gmail.com", "A@googlemail.com",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"b@gmail.com", "a@googlemail.com"}, got)
}

func TestNormalizeRegistrationEmailBlacklist_RejectsInvalidEntries(t *testing.T) {
	_, err := NormalizeRegistrationEmailBlacklist([]string{"ok@gmail.com", "bad", "also bad@gmail.com"})
	require.Error(t, err)
	appErr := infraerrors.FromError(err)
	require.Equal(t, "EMAIL_BLACKLIST_INVALID_ENTRY", appErr.Reason)
	require.Equal(t, "2", appErr.Metadata["invalid_count"])
	require.Contains(t, appErr.Message, "bad")
}

func TestNormalizeRegistrationEmailBlacklist_RejectsOversizedList(t *testing.T) {
	emails := make([]string, MaxRegistrationEmailBlacklistSize+1)
	for i := range emails {
		emails[i] = fmt.Sprintf("u%d@example.com", i)
	}
	_, err := NormalizeRegistrationEmailBlacklist(emails)
	require.Error(t, err)
	require.Equal(t, "EMAIL_BLACKLIST_TOO_LARGE", infraerrors.FromError(err).Reason)

	got, err := NormalizeRegistrationEmailBlacklist(emails[:MaxRegistrationEmailBlacklistSize])
	require.NoError(t, err)
	require.Len(t, got, MaxRegistrationEmailBlacklistSize)
}

func TestParseRegistrationEmailBlacklist_SkipsInvalidAndMalformed(t *testing.T) {
	require.Equal(t, []string{}, ParseRegistrationEmailBlacklist(""))
	require.Equal(t, []string{}, ParseRegistrationEmailBlacklist("not json"))
	require.Equal(t,
		[]string{"a@gmail.com", "b@example.com"},
		ParseRegistrationEmailBlacklist(`["A@gmail.com","bad","b@example.com","a@gmail.com"]`),
	)
}

func TestSettingService_IsRegistrationEmailBlacklisted_MatchesInboxIdentity(t *testing.T) {
	repo := &settingRepoStub{values: map[string]string{
		SettingKeyRegistrationEmailBlacklist: `["john.doe@gmail.com","sales@company.example"]`,
	}}
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	for _, email := range []string{
		"john.doe@gmail.com",
		"JohnDoe@gmail.com",
		"j.o.h.n.d.o.e+spam@googlemail.com",
		"johndoe@gmail.com.",
		"sales@company.example",
		"sales+x@company.example",
	} {
		require.True(t, svc.IsRegistrationEmailBlacklisted(ctx, email), email)
	}
	for _, email := range []string{
		"john.doe2@gmail.com",
		"s.ales@company.example",
		"sales@other.example",
		"not-an-email",
		"",
	} {
		require.False(t, svc.IsRegistrationEmailBlacklisted(ctx, email), email)
	}
}

func TestSettingService_IsRegistrationEmailBlacklisted_RefreshesWhenStoredValueChanges(t *testing.T) {
	repo := &settingRepoStub{values: map[string]string{
		SettingKeyRegistrationEmailBlacklist: `["first@gmail.com"]`,
	}}
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	require.True(t, svc.IsRegistrationEmailBlacklisted(ctx, "first@gmail.com"))
	cached := svc.registrationEmailBlacklistCache.Load()
	require.True(t, svc.IsRegistrationEmailBlacklisted(ctx, "first@gmail.com"))
	require.Same(t, cached, svc.registrationEmailBlacklistCache.Load())

	repo.mu.Lock()
	repo.values[SettingKeyRegistrationEmailBlacklist] = `["second@gmail.com"]`
	repo.mu.Unlock()
	require.False(t, svc.IsRegistrationEmailBlacklisted(ctx, "first@gmail.com"))
	require.True(t, svc.IsRegistrationEmailBlacklisted(ctx, "second@gmail.com"))
}

func TestSettingService_IsRegistrationEmailBlacklisted_ReadFailureDoesNotBlock(t *testing.T) {
	svc := NewSettingService(&settingRepoStub{err: errors.New("db down")}, &config.Config{})
	require.False(t, svc.IsRegistrationEmailBlacklisted(context.Background(), "anyone@gmail.com"))

	missing := NewSettingService(&settingRepoStub{values: map[string]string{}}, &config.Config{})
	require.False(t, missing.IsRegistrationEmailBlacklisted(context.Background(), "anyone@gmail.com"))
}

func TestSettingService_SetRegistrationEmailBlacklist_ReportsAddedAndPersists(t *testing.T) {
	repo := newWritableSettingRepoStub(map[string]string{
		SettingKeyRegistrationEmailBlacklist: `["old@gmail.com","keep@gmail.com"]`,
	})
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	saved, added, err := svc.SetRegistrationEmailBlacklist(ctx, []string{"Keep@gmail.com", "new@gmail.com", "new@gmail.com"})
	require.NoError(t, err)
	require.Equal(t, []string{"keep@gmail.com", "new@gmail.com"}, saved)
	require.Equal(t, []string{"new@gmail.com"}, added)

	var stored []string
	require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyRegistrationEmailBlacklist]), &stored))
	require.Equal(t, saved, stored)
	require.True(t, svc.IsRegistrationEmailBlacklisted(ctx, "n.e.w@googlemail.com"))
	require.False(t, svc.IsRegistrationEmailBlacklisted(ctx, "old@gmail.com"))

	saved, added, err = svc.SetRegistrationEmailBlacklist(ctx, []string{})
	require.NoError(t, err)
	require.Empty(t, saved)
	require.Empty(t, added)
	require.Equal(t, "[]", repo.values[SettingKeyRegistrationEmailBlacklist])
}

func TestSettingService_SetRegistrationEmailBlacklist_InvalidEntryDoesNotWrite(t *testing.T) {
	repo := newWritableSettingRepoStub(map[string]string{
		SettingKeyRegistrationEmailBlacklist: `["old@gmail.com"]`,
	})
	svc := NewSettingService(repo, &config.Config{})

	_, _, err := svc.SetRegistrationEmailBlacklist(context.Background(), []string{"ok@gmail.com", "broken"})
	require.Error(t, err)
	require.Zero(t, repo.setCalls)
	require.Equal(t, `["old@gmail.com"]`, repo.values[SettingKeyRegistrationEmailBlacklist])
}

func TestSettingService_SetRegistrationEmailBlacklist_ReadFailureDoesNotWrite(t *testing.T) {
	repo := newWritableSettingRepoStub(nil)
	repo.err = errors.New("db down")
	svc := NewSettingService(repo, &config.Config{})

	_, _, err := svc.SetRegistrationEmailBlacklist(context.Background(), []string{"ok@gmail.com"})
	require.Error(t, err)
	require.Zero(t, repo.setCalls)
}
