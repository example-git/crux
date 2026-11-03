package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/example-git/crux/internal/db"
)

type CatalogEntry struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	MessageCount int64  `json:"message_count"`
	UpdatedAt    int64  `json:"updated_at"`
}

type Catalog struct {
	Sessions []CatalogEntry `json:"sessions"`
	More     bool           `json:"more"`
}

func ReadCatalog(ctx context.Context, dataDir string, offset int) (Catalog, error) {
	result := Catalog{Sessions: []CatalogEntry{}}
	if offset < 0 {
		return result, errors.New("invalid session offset")
	}
	database := filepath.Join(dataDir, "crux.db")
	if _, err := os.Stat(database); os.IsNotExist(err) {
		return result, nil
	} else if err != nil {
		return result, errors.New("project session database is unavailable")
	}
	conn, err := db.ConnectReadOnly(ctx, database)
	if err != nil {
		return result, errors.New("could not open project session database")
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, "SELECT id, title, message_count, updated_at FROM sessions WHERE parent_session_id IS NULL AND archived = 0 ORDER BY updated_at DESC, id LIMIT 101 OFFSET ?", offset)
	if err != nil {
		return result, errors.New("could not read project sessions")
	}
	defer rows.Close()
	for rows.Next() {
		var item CatalogEntry
		if err := rows.Scan(&item.ID, &item.Title, &item.MessageCount, &item.UpdatedAt); err != nil {
			return result, errors.New("could not read project session metadata")
		}
		result.Sessions = append(result.Sessions, item)
	}
	if err := rows.Err(); err != nil {
		return result, errors.New("could not finish reading project sessions")
	}
	if len(result.Sessions) > 100 {
		result.More = true
		result.Sessions = result.Sessions[:100]
	}
	return result, nil
}
