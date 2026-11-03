package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.UserEmailBlacklistRepository = (*userRepository)(nil)

// listActiveUsersByEmailAliasProbeKeysSQL 把每个用户邮箱折成与 service.EmailAliasProbeKey
// 相同的形态（小写、去首尾空白与域名根点、本地部分去 "+后缀"、本地与域名都去点）后与探针键做
// 哈希连接；去点会在 Gmail 系之外过度匹配，由 service 层按收件箱身份复核。
const listActiveUsersByEmailAliasProbeKeysSQL = `
WITH probes AS (
	SELECT DISTINCT unnest($1::text[]) AS probe
), candidates AS (
	SELECT id, email, username,
		split_part(RTRIM(LOWER(TRIM(email)), '.'), '@', 1) AS local_part,
		split_part(RTRIM(LOWER(TRIM(email)), '.'), '@', 2) AS domain_part
	FROM users
	WHERE deleted_at IS NULL AND status = $2 AND role <> $3
)
SELECT c.id, c.email, c.username
FROM candidates c
JOIN probes p ON p.probe =
	REPLACE(
		CASE WHEN strpos(c.local_part, '+') > 1 THEN split_part(c.local_part, '+', 1) ELSE c.local_part END,
		'.', ''
	) || '@' || REPLACE(c.domain_part, '.', '')
ORDER BY c.id`

// ListActiveUsersByEmailAliasProbeKeys 见 service.UserEmailBlacklistRepository。
func (r *userRepository) ListActiveUsersByEmailAliasProbeKeys(ctx context.Context, keys []string) ([]service.User, error) {
	if len(keys) == 0 {
		return []service.User{}, nil
	}
	rows, err := r.sql.QueryContext(ctx, listActiveUsersByEmailAliasProbeKeysSQL, pq.Array(keys), service.StatusActive, service.RoleAdmin)
	if err != nil {
		return nil, fmt.Errorf("list users by email alias probes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	users := make([]service.User, 0)
	for rows.Next() {
		var user service.User
		if err := rows.Scan(&user.ID, &user.Email, &user.Username); err != nil {
			return nil, fmt.Errorf("scan user by email alias probe: %w", err)
		}
		user.Status = service.StatusActive
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users by email alias probes: %w", err)
	}
	return users, nil
}

// BatchDisableUsers 见 service.UserEmailBlacklistRepository。
func (r *userRepository) BatchDisableUsers(ctx context.Context, userIDs []int64) ([]int64, error) {
	if len(userIDs) == 0 {
		return []int64{}, nil
	}
	rows, err := r.sql.QueryContext(ctx, `
UPDATE users SET status = $1, updated_at = NOW()
WHERE id = ANY($2) AND deleted_at IS NULL AND role <> $3 AND status <> $1
RETURNING id`, service.StatusDisabled, pq.Array(userIDs), service.RoleAdmin)
	if err != nil {
		return nil, fmt.Errorf("batch disable users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	disabled := make([]int64, 0, len(userIDs))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan disabled user id: %w", err)
		}
		disabled = append(disabled, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate disabled user ids: %w", err)
	}
	return disabled, nil
}
