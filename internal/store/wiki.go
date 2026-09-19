package store

import "context"

// WikiRefs is what ForgeSync last wrote to a node's wiki.
func (s *Store) WikiRefs(ctx context.Context, repositoryID, node string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ref, sha FROM wiki_refs WHERE repository_id = $1::uuid AND node = $2`, repositoryID, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := map[string]string{}
	for rows.Next() {
		var ref, sha string
		if err := rows.Scan(&ref, &sha); err != nil {
			return nil, err
		}
		refs[ref] = sha
	}
	return refs, rows.Err()
}

// SaveWikiRefs replaces them.
func (s *Store) SaveWikiRefs(ctx context.Context, repositoryID, node string, refs map[string]string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM wiki_refs WHERE repository_id = $1::uuid AND node = $2`, repositoryID, node); err != nil {
		return err
	}
	for ref, sha := range refs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO wiki_refs (repository_id, node, ref, sha) VALUES ($1::uuid, $2, $3, $4)`,
			repositoryID, node, ref, sha); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
