package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// txTimeout bounds a single loft.db transaction (acquire + token fetch + statements), so a slow or
// exhausted pool fails the request fast instead of pinning its goroutine indefinitely.
const txTimeout = 15 * time.Second

const schemaSQL = `
create table if not exists documents (
  site       text  not null,
  collection text  not null,
  id         uuid  not null default gen_random_uuid(),
  doc        jsonb not null,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  primary key (site, collection, id)
);
alter table documents add column if not exists creator text;
create index if not exists documents_site_coll_created on documents (site, collection, created_at desc);
alter table documents enable row level security;
alter table documents force row level security;
do $$ begin
  if not exists (select 1 from pg_policies where tablename = 'documents' and policyname = 'tenant') then
    create policy tenant on documents
      using (site = current_setting('loft.site', true))
      with check (site = current_setting('loft.site', true));
  end if;
end $$;
create table if not exists collections (
  site       text not null,
  name       text not null,
  owner_only boolean not null default false,
  primary key (site, name)
);
alter table collections enable row level security;
alter table collections force row level security;
do $$ begin
  if not exists (select 1 from pg_policies where tablename = 'collections' and policyname = 'tenant') then
    create policy tenant on collections
      using (site = current_setting('loft.site', true))
      with check (site = current_setting('loft.site', true));
  end if;
end $$;
`

func (s *Store) ensureSchema(ctx context.Context) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaDone {
		return nil
	}
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return err
	}
	s.schemaDone = true
	return nil
}

// withSite runs fn in a transaction whose loft.site setting confines every row via RLS.
func (s *Store) withSite(ctx context.Context, site string, fn func(pgx.Tx) error) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, txTimeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if _, err := tx.Exec(ctx, "select set_config('loft.site', $1, true)", site); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
