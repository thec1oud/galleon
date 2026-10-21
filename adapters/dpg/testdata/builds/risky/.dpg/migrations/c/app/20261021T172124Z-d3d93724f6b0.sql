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
  SELECT status, checksum INTO r FROM "dpg".migrations WHERE log = 'app' AND id = '20261021T172124Z-d3d93724f6b0';
  IF FOUND THEN
    IF r.checksum <> 'sha256:9c64e84de15b45205d29b9eb4eb49721d4d44547993b4e0807714648fee703ce' THEN
      RAISE EXCEPTION 'DPG-E049: migration % was recorded in this database with different statements', '20261021T172124Z-d3d93724f6b0';
    END IF;
    IF r.status = 'complete' THEN
      RAISE EXCEPTION 'DPG-E049: migration % has already been applied to this database', '20261021T172124Z-d3d93724f6b0';
    END IF;
    IF r.status = 'committed' THEN
      RAISE EXCEPTION 'DPG-E049: migration % failed after its transaction committed; dpg apply --resume finishes it', '20261021T172124Z-d3d93724f6b0';
    END IF;
  END IF;
  SELECT count(*), min(m.id) INTO n, head FROM "dpg".migrations m
    WHERE m.log = 'app' AND m.status = 'complete'
      AND NOT EXISTS (SELECT 1 FROM "dpg".migrations c WHERE c.log = m.log AND c.parent = m.id AND c.status = 'complete');
  IF n > 1 THEN
    RAISE EXCEPTION 'DPG-E050: the ledger of % has % last migrations', 'app', n;
  END IF;
  IF coalesce(head, '') <> '20261021T172124Z-e6bff4b6c8fa' THEN
    RAISE EXCEPTION 'DPG-E050: migration % follows %, but the last migration this database ran is %', '20261021T172124Z-d3d93724f6b0', '20261021T172124Z-e6bff4b6c8fa', coalesce(head, '(none)');
  END IF;
  INSERT INTO "dpg".migrations (log, id, parent, checksum, dpg_version, status)
    VALUES ('app', '20261021T172124Z-d3d93724f6b0', '20261021T172124Z-e6bff4b6c8fa', 'sha256:9c64e84de15b45205d29b9eb4eb49721d4d44547993b4e0807714648fee703ce', 'dev', 'started')
    ON CONFLICT (log, id) DO UPDATE SET started_at = now();
END
$dpg$;

DO $dpg$
DECLARE
  d text;
BEGIN
  SELECT string_agg(owner, ', ' ORDER BY owner) INTO d FROM (SELECT DISTINCT owner FROM (
  SELECT CASE d.classid
    WHEN 'pg_rewrite'::regclass THEN (SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT r.ev_class FROM pg_rewrite r WHERE r.oid = d.objid))
    WHEN 'pg_class'::regclass THEN coalesce((SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT i.indrelid FROM pg_index i WHERE i.indexrelid = d.objid)), (SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = d.objid))
    WHEN 'pg_constraint'::regclass THEN coalesce((SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT nullif(k.conrelid, 0) FROM pg_constraint k WHERE k.oid = d.objid)), (SELECT n.nspname || '.' || t.typname FROM pg_constraint k JOIN pg_type t ON t.oid = k.contypid JOIN pg_namespace n ON n.oid = t.typnamespace WHERE k.oid = d.objid))
    WHEN 'pg_trigger'::regclass THEN (SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT t.tgrelid FROM pg_trigger t WHERE t.oid = d.objid))
    WHEN 'pg_attrdef'::regclass THEN (SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT a.adrelid FROM pg_attrdef a WHERE a.oid = d.objid))
    WHEN 'pg_proc'::regclass THEN (SELECT n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE p.oid = d.objid)
    WHEN 'pg_type'::regclass THEN (SELECT n.nspname || '.' || t.typname FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE t.oid = d.objid)
    ELSE pg_describe_object(d.classid, d.objid, d.objsubid)
  END AS owner
  FROM pg_depend d
  WHERE d.refclassid = 'pg_class'::regclass AND d.refobjid = (SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'a') AND d.deptype = 'n'
) x WHERE owner IS NOT NULL AND owner <> 'public.a') x
    WHERE owner NOT IN ('public.a', 'public.b')
      AND NOT (owner LIKE '%(%' AND split_part(owner, '(', 1) || '(' IN (SELECT split_part(k, '(', 1) || '(' FROM unnest(ARRAY['public.a', 'public.b']::text[]) k));
  IF d IS NOT NULL THEN
    RAISE EXCEPTION 'DPG-E052: the migration drops table public.a, but % depends on it and DPG doesn''t manage it', d;
  END IF;
END
$dpg$;

DO $dpg$
DECLARE
  r record;
  sp text;
  fp text;
  fp_oid oid;
BEGIN
  IF coalesce(current_setting('dpg.allow_drift', true), '') = 'on' THEN
    RETURN;
  END IF;
  SELECT fingerprint, pg_major INTO r FROM "dpg".objects WHERE log = 'app' AND kind = 'table' AND key = 'public.a';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'DPG-E054: DPG has no record of table public.a in this database: dpg verify (connected) records it when it has no drift, or SET dpg.allow_drift = on runs the migration anyway';
  END IF;
  IF r.pg_major <> current_setting('server_version_num')::int / 10000 THEN
    RAISE EXCEPTION 'DPG-E054: the record of table public.a was taken on PostgreSQL %: dpg verify (connected) records it again', r.pg_major;
  END IF;
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
	WHERE n.nspname = 'public' AND c.relname = 'a');
  PERFORM set_config('search_path', sp, true);
  IF fp IS DISTINCT FROM r.fingerprint THEN
    RAISE EXCEPTION 'DPG-E055: table public.a changed since DPG last saw it: dpg verify shows how';
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
  PERFORM 1 FROM "dpg".migrations WHERE log = 'app' AND id = '20261021T172124Z-d3d93724f6b0' AND status = 'started' AND checksum = 'sha256:9c64e84de15b45205d29b9eb4eb49721d4d44547993b4e0807714648fee703ce' FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'DPG-E049: migration % is not waiting to run in this database (another run applied it)', '20261021T172124Z-d3d93724f6b0';
  END IF;
  SELECT count(*), min(m.id) INTO n, head FROM "dpg".migrations m
    WHERE m.log = 'app' AND m.status = 'complete'
      AND NOT EXISTS (SELECT 1 FROM "dpg".migrations c WHERE c.log = m.log AND c.parent = m.id AND c.status = 'complete');
  IF n > 1 THEN
    RAISE EXCEPTION 'DPG-E050: the ledger of % has % last migrations', 'app', n;
  END IF;
  IF coalesce(head, '') <> '20261021T172124Z-e6bff4b6c8fa' THEN
    RAISE EXCEPTION 'DPG-E050: migration % follows %, but the last migration this database ran is %', '20261021T172124Z-d3d93724f6b0', '20261021T172124Z-e6bff4b6c8fa', coalesce(head, '(none)');
  END IF;
END
$dpg$;

DO $dpg$
DECLARE
  d text;
BEGIN
  SELECT string_agg(owner, ', ' ORDER BY owner) INTO d FROM (SELECT DISTINCT owner FROM (
  SELECT CASE d.classid
    WHEN 'pg_rewrite'::regclass THEN (SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT r.ev_class FROM pg_rewrite r WHERE r.oid = d.objid))
    WHEN 'pg_class'::regclass THEN coalesce((SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT i.indrelid FROM pg_index i WHERE i.indexrelid = d.objid)), (SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = d.objid))
    WHEN 'pg_constraint'::regclass THEN coalesce((SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT nullif(k.conrelid, 0) FROM pg_constraint k WHERE k.oid = d.objid)), (SELECT n.nspname || '.' || t.typname FROM pg_constraint k JOIN pg_type t ON t.oid = k.contypid JOIN pg_namespace n ON n.oid = t.typnamespace WHERE k.oid = d.objid))
    WHEN 'pg_trigger'::regclass THEN (SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT t.tgrelid FROM pg_trigger t WHERE t.oid = d.objid))
    WHEN 'pg_attrdef'::regclass THEN (SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = (SELECT a.adrelid FROM pg_attrdef a WHERE a.oid = d.objid))
    WHEN 'pg_proc'::regclass THEN (SELECT n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE p.oid = d.objid)
    WHEN 'pg_type'::regclass THEN (SELECT n.nspname || '.' || t.typname FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE t.oid = d.objid)
    ELSE pg_describe_object(d.classid, d.objid, d.objsubid)
  END AS owner
  FROM pg_depend d
  WHERE d.refclassid = 'pg_class'::regclass AND d.refobjid = (SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'a') AND d.deptype = 'n'
) x WHERE owner IS NOT NULL AND owner <> 'public.a') x
    WHERE owner NOT IN ('public.a', 'public.b')
      AND NOT (owner LIKE '%(%' AND split_part(owner, '(', 1) || '(' IN (SELECT split_part(k, '(', 1) || '(' FROM unnest(ARRAY['public.a', 'public.b']::text[]) k));
  IF d IS NOT NULL THEN
    RAISE EXCEPTION 'DPG-E052: the migration drops table public.a, but % depends on it and DPG doesn''t manage it', d;
  END IF;
END
$dpg$;

DO $dpg$
DECLARE
  r record;
  sp text;
  fp text;
  fp_oid oid;
BEGIN
  IF coalesce(current_setting('dpg.allow_drift', true), '') = 'on' THEN
    RETURN;
  END IF;
  SELECT fingerprint, pg_major INTO r FROM "dpg".objects WHERE log = 'app' AND kind = 'table' AND key = 'public.a';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'DPG-E054: DPG has no record of table public.a in this database: dpg verify (connected) records it when it has no drift, or SET dpg.allow_drift = on runs the migration anyway';
  END IF;
  IF r.pg_major <> current_setting('server_version_num')::int / 10000 THEN
    RAISE EXCEPTION 'DPG-E054: the record of table public.a was taken on PostgreSQL %: dpg verify (connected) records it again', r.pg_major;
  END IF;
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
	WHERE n.nspname = 'public' AND c.relname = 'a');
  PERFORM set_config('search_path', sp, true);
  IF fp IS DISTINCT FROM r.fingerprint THEN
    RAISE EXCEPTION 'DPG-E055: table public.a changed since DPG last saw it: dpg verify shows how';
  END IF;
END
$dpg$;

-- safety: DESTRUCTIVE
DROP TABLE IF EXISTS "public"."a";

DELETE FROM "dpg".objects WHERE log = 'app' AND kind = 'table' AND key = 'public.a';

UPDATE "dpg".migrations SET status = 'complete', finished_at = now() WHERE log = 'app' AND id = '20261021T172124Z-d3d93724f6b0';

COMMIT;
