package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 注册邮箱精确黑名单。
//
// 名单项是完整邮箱地址，匹配按收件箱身份（NormalizeEmailForAliasDedup）进行：
// 拉黑 a.b@gmail.com 同时拦截 ab@googlemail.com、a.b+x@gmail.com 等投递到同一收件箱的
// 变体；Gmail 系之外点号仍有意义，只折叠 "+后缀"。名单原样保存管理员录入的地址，
// 身份归一只发生在匹配时。

// MaxRegistrationEmailBlacklistSize 是精确黑名单的条数上限。
const MaxRegistrationEmailBlacklistSize = 20000

const (
	registrationEmailBlacklistLocalMaxLen = 64
	registrationEmailBlacklistMaxLen      = 254
	registrationEmailBlacklistErrorSample = 10
)

var ErrEmailBlocked = infraerrors.BadRequest("EMAIL_BLOCKED", "this email address is not allowed")

// NormalizeRegistrationEmailBlacklistEntry 把一条录入归一为小写、去首尾空白、去域名根点的
// 邮箱地址；格式不合法时返回 false。
func NormalizeRegistrationEmailBlacklistEntry(raw string) (string, bool) {
	local, domain, ok := splitEmailForPolicy(raw)
	if !ok {
		return "", false
	}
	if len(local) > registrationEmailBlacklistLocalMaxLen || !isValidRegistrationEmailDomain(domain) {
		return "", false
	}
	for _, r := range local {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", false
		}
	}
	email := local + "@" + domain
	if len(email) > registrationEmailBlacklistMaxLen {
		return "", false
	}
	return email, true
}

// NormalizeRegistrationEmailBlacklist 校验并归一管理员提交的名单：按归一后的地址去重并保持
// 原有顺序；任一项格式不合法或超过条数上限都整体拒绝。
func NormalizeRegistrationEmailBlacklist(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	var invalid []string
	for _, item := range raw {
		if strings.TrimSpace(item) == "" {
			continue
		}
		email, ok := NormalizeRegistrationEmailBlacklistEntry(item)
		if !ok {
			invalid = append(invalid, strings.TrimSpace(item))
			continue
		}
		if _, dup := seen[email]; dup {
			continue
		}
		seen[email] = struct{}{}
		out = append(out, email)
	}
	if len(invalid) > 0 {
		sample := invalid
		if len(sample) > registrationEmailBlacklistErrorSample {
			sample = sample[:registrationEmailBlacklistErrorSample]
		}
		return nil, infraerrors.BadRequest(
			"EMAIL_BLACKLIST_INVALID_ENTRY",
			fmt.Sprintf("invalid email address (%d): %s", len(invalid), strings.Join(sample, ", ")),
		).WithMetadata(map[string]string{
			"invalid_count":   strconv.Itoa(len(invalid)),
			"invalid_entries": strings.Join(sample, ","),
		})
	}
	if len(out) > MaxRegistrationEmailBlacklistSize {
		return nil, infraerrors.BadRequest(
			"EMAIL_BLACKLIST_TOO_LARGE",
			fmt.Sprintf("email blacklist cannot exceed %d entries", MaxRegistrationEmailBlacklistSize),
		).WithMetadata(map[string]string{
			"max": strconv.Itoa(MaxRegistrationEmailBlacklistSize),
		})
	}
	return out, nil
}

// ParseRegistrationEmailBlacklist 解析已持久化的 JSON 名单；非法项被跳过，不影响其余项生效。
func ParseRegistrationEmailBlacklist(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}
	}
	var items []string
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return []string{}
	}
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		email, ok := NormalizeRegistrationEmailBlacklistEntry(item)
		if !ok {
			continue
		}
		if _, dup := seen[email]; dup {
			continue
		}
		seen[email] = struct{}{}
		out = append(out, email)
	}
	return out
}

// RegistrationEmailBlacklistIdentities 返回名单项对应的收件箱身份集合。
func RegistrationEmailBlacklistIdentities(emails []string) map[string]struct{} {
	identities := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		if identity := NormalizeEmailForAliasDedup(email); identity != "" {
			identities[identity] = struct{}{}
		}
	}
	return identities
}

// registrationEmailBlacklistIndex 是某一版持久化 JSON 的解析结果。
type registrationEmailBlacklistIndex struct {
	raw        string
	identities map[string]struct{}
}

func (idx *registrationEmailBlacklistIndex) contains(email string) bool {
	if idx == nil || len(idx.identities) == 0 {
		return false
	}
	if _, _, ok := splitEmailForPolicy(email); !ok {
		return false
	}
	_, hit := idx.identities[NormalizeEmailForAliasDedup(email)]
	return hit
}

func (s *SettingService) registrationEmailBlacklistIndexFor(raw string) *registrationEmailBlacklistIndex {
	if cached, ok := s.registrationEmailBlacklistCache.Load().(*registrationEmailBlacklistIndex); ok && cached != nil && cached.raw == raw {
		return cached
	}
	idx := &registrationEmailBlacklistIndex{
		raw:        raw,
		identities: RegistrationEmailBlacklistIdentities(ParseRegistrationEmailBlacklist(raw)),
	}
	s.registrationEmailBlacklistCache.Store(idx)
	return idx
}

// GetRegistrationEmailBlacklist 返回已保存的精确邮箱黑名单。
func (s *SettingService) GetRegistrationEmailBlacklist(ctx context.Context) ([]string, error) {
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRegistrationEmailBlacklist)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("get registration email blacklist: %w", err)
	}
	return ParseRegistrationEmailBlacklist(raw), nil
}

// SetRegistrationEmailBlacklist 整体替换精确邮箱黑名单，返回保存后的名单与相对旧名单新增的项。
func (s *SettingService) SetRegistrationEmailBlacklist(ctx context.Context, emails []string) (saved []string, added []string, err error) {
	normalized, err := NormalizeRegistrationEmailBlacklist(emails)
	if err != nil {
		return nil, nil, err
	}
	previous, err := s.GetRegistrationEmailBlacklist(ctx)
	if err != nil {
		return nil, nil, err
	}
	previousSet := make(map[string]struct{}, len(previous))
	for _, email := range previous {
		previousSet[email] = struct{}{}
	}
	added = make([]string, 0)
	for _, email := range normalized {
		if _, existed := previousSet[email]; !existed {
			added = append(added, email)
		}
	}

	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal registration email blacklist: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyRegistrationEmailBlacklist, string(data)); err != nil {
		return nil, nil, fmt.Errorf("save registration email blacklist: %w", err)
	}
	s.registrationEmailBlacklistIndexFor(string(data))
	return normalized, added, nil
}

// IsRegistrationEmailBlacklisted 判断邮箱（含同一收件箱的别名变体）是否在精确黑名单中。
// 读取失败时不拦截，与域名黑名单的读取口径一致。
func (s *SettingService) IsRegistrationEmailBlacklisted(ctx context.Context, email string) bool {
	if s == nil || s.settingRepo == nil {
		return false
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyRegistrationEmailBlacklist)
	if err != nil {
		return false
	}
	return s.registrationEmailBlacklistIndexFor(raw).contains(email)
}
