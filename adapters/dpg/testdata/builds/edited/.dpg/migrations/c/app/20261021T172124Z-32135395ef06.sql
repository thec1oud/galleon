-- DPG Migration
-- Generated:       2026-10-21T17:21:24Z
-- Cluster:         c
-- Database:        app

-- non-transactional, before the transaction

CREATE SCHEMA IF NOT EXISTS "dpg";

CREATE TABLE IF NOT EXISTS "dpg".migrations (
    log         text NOT NULL,
    id          text NOT NULL,
    parent      text NOT NULL,
    checksum    text NOT NULL,
    dpg_version text NOT NULL,
    status      text NOT NULL CHECK (status IN ('started', 'committed', 'complete')),
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    PRIMARY KEY (log, id)
);

CREATE TABLE IF NOT EXISTS "dpg".objects (
    log         text NOT NULL,
    kind        text NOT NULL,
    key         text NOT NULL,
    fingerprint text NOT NULL,
    pg_major    int NOT NULL,
    migration   text NOT NULL,
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (log, kind, key)
);

DO $dpg$
DECLARE
  r record;
  n int;
  head text;
BEGIN
  SELECT status, checksum INTO r FROM "dpg".migrations WHERE log = 'app' AND id = '20261021T172124Z-32135395ef06';
  IF FOUND THEN
    IF r.checksum <> 'sha256:5cbf5f557885e837019ec8eae1ce834e1b8095d9e4d5e04bedfbbdc46c058eb1' THEN
      RAISE EXCEPTION 'DPG-E049: migration % was recorded in this database with different statements', '20261021T172124Z-32135395ef06';
    END IF;
    IF r.status = 'complete' THEN
      RAISE EXCEPTION 'DPG-E049: migration % has already been applied to this database', '20261021T172124Z-32135395ef06';
    END IF;
    IF r.status = 'committed' THEN
      RAISE EXCEPTION 'DPG-E049: migration % failed after its transaction committed; dpg apply --resume finishes it', '20261021T172124Z-32135395ef06';
    END IF;
  END IF;
  SELECT count(*), min(m.id) INTO n, head FROM "dpg".migrations m
    WHERE m.log = 'app' AND m.status = 'complete'
      AND NOT EXISTS (SELECT 1 FROM "dpg".migrations c WHERE c.log = m.log AND c.parent = m.id AND c.status = 'complete');
  IF n > 1 THEN
    RAISE EXCEPTION 'DPG-E050: the ledger of % has % last migrations', 'app', n;
  END IF;
  IF coalesce(head, '') <> '20261021T172124Z-df2178bb70a5' THEN
    RAISE EXCEPTION 'DPG-E050: migration % follows %, but the last migration this database ran is %', '20261021T172124Z-32135395ef06', '20261021T172124Z-df2178bb70a5', coalesce(head, '(none)');
  END IF;
  INSERT INTO "dpg".migrations (log, id, parent, checksum, dpg_version, status)
    VALUES ('app', '20261021T172124Z-32135395ef06', '20261021T172124Z-df2178bb70a5', 'sha256:5cbf5f557885e837019ec8eae1ce834e1b8095d9e4d5e04bedfbbdc46c058eb1', 'dev', 'started')
    ON CONFLICT (log, id) DO UPDATE SET started_at = now();
END
$dpg$;

DO $dpg$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'b') THEN
    RAISE EXCEPTION 'DPG-E051: the migration creates table public.b, which already exists in this database (DPG doesn''t manage it: dpg resolve can adopt or ignore it)';
  END IF;
END
$dpg$;

--------

-- transactional
BEGIN;

DO $dpg$
DECLARE
  n int;
  head text;
BEGIN
  PERFORM 1 FROM "dpg".migrations WHERE log = 'app' AND id = '20261021T172124Z-32135395ef06' AND status = 'started' AND checksum = 'sha256:5cbf5f557885e837019ec8eae1ce834e1b8095d9e4d5e04bedfbbdc46c058eb1' FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'DPG-E049: migration % is not waiting to run in this database (another run applied it)', '20261021T172124Z-32135395ef06';
  END IF;
  SELECT count(*), min(m.id) INTO n, head FROM "dpg".migrations m
    WHERE m.log = 'app' AND m.status = 'complete'
      AND NOT EXISTS (SELECT 1 FROM "dpg".migrations c WHERE c.log = m.log AND c.parent = m.id AND c.status = 'complete');
  IF n > 1 THEN
    RAISE EXCEPTION 'DPG-E050: the ledger of % has % last migrations', 'app', n;
  END IF;
  IF coalesce(head, '') <> '20261021T172124Z-df2178bb70a5' THEN
    RAISE EXCEPTION 'DPG-E050: migration % follows %, but the last migration this database ran is %', '20261021T172124Z-32135395ef06', '20261021T172124Z-df2178bb70a5', coalesce(head, '(none)');
  END IF;
END
$dpg$;

DO $dpg$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'b') THEN
    RAISE EXCEPTION 'DPG-E051: the migration creates table public.b, which already exists in this database (DPG doesn''t manage it: dpg resolve can adopt or ignore it)';
  END IF;
END
$dpg$;

CREATE TABLE "public"."b" (
    "id" integer PRIMARY KEY
);

DO $dpg$
DECLARE
  sp text;
  fp text;
  fp_oid oid;
BEGIN
  sp := current_setting('search_path');
  PERFORM set_config('search_path', 'pg_catalog', true);
  fp := (SELECT md5(concat_ws(E'\n', concat_ws('|', c.relkind, c.relpersistence, pg_get_userbyid(c.relowner), c.relacl::text,
				c.relrowsecurity, c.relforcerowsecurity, array_to_string(c.reloptions, ','),
				obj_description(c.oid, 'pg_class'), c.relreplident,
				(SELECT t.spcname FROM pg_tablespace t WHERE t.oid = c.reltablespace),
				CASE WHEN c.relkind IN ('p') THEN pg_get_partkeydef(c.oid) END,
				CASE WHEN c.relispartition THEN pg_get_expr(c.relpartbound, c.oid) END,
				CASE WHEN c.relkind IN ('v', 'm') THEN pg_get_viewdef(c.oid) END,
				(SELECT a.amname FROM pg_am a WHERE a.oid = c.relam),
				(SELECT string_agg(i.inhparent::regclass::text, ',' ORDER BY i.inhseqno) FROM pg_inherits i WHERE i.inhrelid = c.oid)),
		(SELECT string_agg(concat_ws('|', a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
				pg_get_expr(d.adbin, d.adrelid), a.attidentity, a.attgenerated, a.attacl::text,
				col_description(c.oid, a.attnum), a.attstorage, a.attcompression,
				(SELECT co.collname FROM pg_collation co WHERE co.oid = a.attcollation AND a.attcollation <> 0 AND co.collname <> 'default'),
				array_to_string(a.attoptions, ','), array_to_string(a.attfdwoptions, ',')), E'\n' ORDER BY a.attnum)
			FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped),
		(SELECT string_agg(concat_ws('|', k.conname, pg_get_constraintdef(k.oid), obj_description(k.oid, 'pg_constraint')), E'\n' ORDER BY k.conname)
			FROM pg_constraint k WHERE k.conrelid = c.oid),
		(SELECT string_agg(concat_ws('|', pg_get_indexdef(i.indexrelid), obj_description(i.indexrelid, 'pg_class')), E'\n' ORDER BY 1)
			FROM pg_index i WHERE i.indrelid = c.oid),
		(SELECT string_agg(concat_ws('|', t.tgname, pg_get_triggerdef(t.oid), t.tgenabled, obj_description(t.oid, 'pg_trigger')), E'\n' ORDER BY t.tgname)
			FROM pg_trigger t WHERE t.tgrelid = c.oid AND NOT t.tgisinternal),
		(SELECT string_agg(concat_ws('|', p.polname, p.polcmd, p.polpermissive, p.polroles::regrole[]::text,
				pg_get_expr(p.polqual, p.polrelid), pg_get_expr(p.polwithcheck, p.polrelid)), E'\n' ORDER BY p.polname)
			FROM pg_policy p WHERE p.polrelid = c.oid),
		(SELECT concat_ws('|', format_type(s.seqtypid, NULL), s.seqstart, s.seqincrement, s.seqmax, s.seqmin, s.seqcache, s.seqcycle)
			FROM pg_sequence s WHERE s.seqrelid = c.oid),
		(SELECT string_agg(d.refobjid::regclass::text || '.' || a.attname, ',' ORDER BY 1)
			FROM pg_depend d JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
			WHERE c.relkind = 'S' AND d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('a', 'i'))))
	FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = 'public' AND c.relname = 'b');
  PERFORM set_config('search_path', sp, true);
  IF fp IS NOT NULL THEN
    INSERT INTO "dpg".objects (log, kind, key, fingerprint, pg_major, migration, recorded_at)
    VALUES ('app', 'table', 'public.b', fp, current_setting('server_version_num')::int / 10000, '20261021T172124Z-32135395ef06', now())
    ON CONFLICT (log, kind, key) DO UPDATE SET fingerprint = excluded.fingerprint, pg_major = excluded.pg_major,
      migration = excluded.migration, recorded_at = excluded.recorded_at;
  END IF;
END
$dpg$;

UPDATE "dpg".migrations SET status = 'complete', finished_at = now() WHERE log = 'app' AND id = '20261021T172124Z-32135395ef06';

COMMIT;
