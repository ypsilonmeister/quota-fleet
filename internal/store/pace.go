package store

import "context"

// ActivePoolLoads counts assigned jobs that are waiting, queued, or claimed (including running).
func (s *Store) ActivePoolLoads(ctx context.Context) (map[string]int, error) {
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT pool, COUNT(*) FROM jobs
		WHERE pool <> '' AND status IN ('waiting', 'queued', 'claimed')
		GROUP BY pool`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	load := make(map[string]int)
	for rows.Next() {
		var pool string
		var count int
		if err := rows.Scan(&pool, &count); err != nil {
			return nil, err
		}
		load[pool] = count
	}
	return load, rows.Err()
}
