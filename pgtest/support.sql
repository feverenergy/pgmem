-- pgtest's support schema. installSupport runs this file in the template
-- database after Migrate, then runs SELECT _pgtest.capture(), and then
-- creates the event trigger (see support.go). The trigger comes last so
-- that the tables capture() creates do not mark the template dirty. Every
-- per-test database is a copy of the template, so each one carries this
-- schema and the captured seeds.
--
-- The functions set search_path to pg_catalog, so a session's search_path
-- cannot redirect them, and regclass::text always names the schema.

CREATE SCHEMA _pgtest;

-- One row. The DDL event trigger sets dirty. lo_count is the
-- template's number of large objects. files is _pgtest.files() as capture
-- left it.
CREATE TABLE _pgtest.state (
	dirty boolean NOT NULL,
	lo_count bigint NOT NULL,
	files text
);
INSERT INTO _pgtest.state VALUES (false, 0, NULL);

-- A seed is a copy of a table's template rows in _pgtest.s<oid>. cols is
-- the quoted, comma-separated list of the columns that are neither dropped
-- nor generated, in attnum order (empty if there is none).
CREATE TABLE _pgtest.seeds (
	rel regclass PRIMARY KEY,
	copy regclass NOT NULL,
	cols text NOT NULL
);

-- The template's sequence positions, as SELECT last_value, is_called FROM
-- the sequence reads them.
CREATE TABLE _pgtest.seqs (
	seq regclass PRIMARY KEY,
	last_value bigint,
	is_called boolean NOT NULL
);

-- The user relations of kind kinds. These are the relations outside
-- pg_catalog, information_schema, _pgtest, the TOAST schemas and the
-- temporary schemas.
CREATE FUNCTION _pgtest.user_relations(kinds "char"[]) RETURNS SETOF oid
LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
	SELECT c.oid
	FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind = ANY (kinds)
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema', '_pgtest')
	  AND n.nspname NOT LIKE 'pg\_toast%'
	  AND n.nspname NOT LIKE 'pg\_temp%'
$$;

-- Every user table that holds rows itself: plain tables and partitions
-- (relkind r). Partitioned parents (relkind p) hold none.
CREATE FUNCTION _pgtest.user_tables() RETURNS SETOF oid
LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
	SELECT _pgtest.user_relations('{r}')
$$;

-- The quoted, comma-separated list of rel's columns that are neither
-- dropped nor generated, in attnum order. It is empty if there is none.
CREATE FUNCTION _pgtest.cols(rel oid) RETURNS text
LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
	SELECT coalesce(string_agg(quote_ident(attname), ', ' ORDER BY attnum), '')
	FROM pg_attribute
	WHERE attrelid = rel AND attnum > 0 AND NOT attisdropped AND attgenerated = ''
$$;

-- The data file (relfilenode) of each of _pgtest's tables, as oid:file,
-- in oid order. TRUNCATE gives a table a new file, so a changed file
-- reveals a TRUNCATE, which the cumulative statistics do not count.
CREATE FUNCTION _pgtest.files() RETURNS text
LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
	SELECT string_agg(c.oid || ':' || pg_relation_filenode(c.oid), ',' ORDER BY c.oid)
	FROM pg_class c
	WHERE c.relnamespace = '_pgtest'::regnamespace AND c.relkind = 'r'
$$;

-- capture records the template's data: a seed per table with rows, the
-- position of every sequence, the number of large objects, and the files
-- of _pgtest's tables. capture skips a table that was never written,
-- which has size 0, without a scan. It reads rows with ONLY, so a table's
-- seed never holds its inheritance children's rows. Each child has its
-- own seed.
CREATE FUNCTION _pgtest.capture() RETURNS void
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE
	r oid;
	cols text;
	copy_name text;
	has_rows boolean;
	lv bigint;
	called boolean;
BEGIN
	FOR r IN SELECT t FROM _pgtest.user_tables() t WHERE pg_relation_size(t) > 0 ORDER BY t LOOP
		EXECUTE format('SELECT EXISTS (SELECT FROM ONLY %s)', r::regclass) INTO has_rows;
		CONTINUE WHEN NOT has_rows;
		cols := _pgtest.cols(r);
		copy_name := format('_pgtest.%I', 's' || r);
		EXECUTE format('CREATE TABLE %s AS SELECT %s FROM ONLY %s', copy_name, cols, r::regclass);
		INSERT INTO _pgtest.seeds VALUES (r, copy_name::regclass, cols);
	END LOOP;

	FOR r IN SELECT s FROM _pgtest.user_relations('{S}') s ORDER BY s LOOP
		EXECUTE format('SELECT last_value, is_called FROM %s', r::regclass) INTO lv, called;
		INSERT INTO _pgtest.seqs VALUES (r, lv, called);
	END LOOP;

	UPDATE _pgtest.state SET
		lo_count = (SELECT count(*) FROM pg_largeobject_metadata),
		files = _pgtest.files();
END
$$;

-- The DDL guard. The event trigger _pgtest_ddl runs this before every DDL
-- command. The mark is transactional, so DDL that rolls back leaves the
-- database clean. installSupport sets the trigger ENABLE ALWAYS, so it
-- also fires under session_replication_role = replica. It runs as its
-- owner (SECURITY DEFINER), so DDL by a role with no rights on _pgtest,
-- after SET ROLE, still works, and the guard still sees it.
CREATE FUNCTION _pgtest.mark_dirty() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
	UPDATE _pgtest.state SET dirty = true;
END
$$;

-- reset restores the template's data in one transaction, or reports why
-- the database must be dropped instead. It refuses a database with other
-- sessions; one whose test wrote to _pgtest's tables, which the reset
-- relies on; one with DDL since the template, or a disabled or dropped
-- DDL guard; one with database-level settings (ALTER DATABASE ... SET
-- fires no event trigger); and one with large objects. Otherwise it
-- truncates every table that was written (size > 0) and every seeded
-- table, refills the seeded ones from their copies with triggers and
-- foreign-key checks off, and moves back every sequence that moved.
-- Nothing in it is DDL, so it never marks the database dirty, and it
-- writes none of _pgtest's tables.
CREATE FUNCTION _pgtest.reset() RETURNS TABLE (ok boolean, reason text)
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE
	st _pgtest.state;
	list text;
	s _pgtest.seeds;
BEGIN
	IF EXISTS (
		SELECT FROM pg_stat_activity
		WHERE datid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND pid <> pg_backend_pid()
	) THEN
		RETURN QUERY SELECT false, 'other sessions';
		RETURN;
	END IF;

	-- Writes to _pgtest's tables. Each per-test database starts with no
	-- statistics of its own, because installSupport zeroes the template's.
	-- So every row inserted, updated or deleted counts, even in a
	-- transaction that rolled back. The DDL guard's updates of state do not
	-- count. The other sessions have ended, and a backend writes its
	-- statistics before it leaves pg_stat_activity. The statistics do not
	-- count TRUNCATE, but TRUNCATE changes the table's file.
	SELECT * INTO st FROM _pgtest.state;
	IF NOT FOUND OR st.files IS DISTINCT FROM _pgtest.files() OR EXISTS (
		SELECT FROM pg_stat_all_tables
		WHERE schemaname = '_pgtest'
		  AND n_tup_ins + n_tup_del + CASE WHEN relname = 'state' THEN 0 ELSE n_tup_upd END > 0
	) THEN
		RETURN QUERY SELECT false, 'pgtest tables changed';
		RETURN;
	END IF;

	-- DDL since the template, or a guard that may have missed some. The
	-- trigger must still exist and fire always. ALTER EVENT TRIGGER and
	-- DROP EVENT TRIGGER fire no event trigger, so this is the only way to
	-- see them. This cannot detect SET event_triggers = off (superuser,
	-- PG17), because DDL run while it is off leaves no trace.
	IF st.dirty OR NOT EXISTS (
		SELECT FROM pg_event_trigger WHERE evtname = '_pgtest_ddl' AND evtenabled = 'A'
	) THEN
		RETURN QUERY SELECT false, 'ddl';
		RETURN;
	END IF;

	IF EXISTS (
		SELECT FROM pg_db_role_setting
		WHERE setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())
	) THEN
		RETURN QUERY SELECT false, 'database settings';
		RETURN;
	END IF;

	-- reset does not restore large objects. Counts alone would miss a test
	-- that writes into a template's large object, or unlinks one and
	-- creates another. So a template that has any is never reset, and
	-- otherwise any large object means the test created one.
	IF st.lo_count > 0 OR (SELECT count(*) FROM pg_largeobject_metadata) <> st.lo_count THEN
		RETURN QUERY SELECT false, 'large objects';
		RETURN;
	END IF;

	-- Under replica, user triggers and foreign-key checks are off until commit.
	PERFORM set_config('session_replication_role', 'replica', true);

	SELECT string_agg(t::regclass::text, ', ') INTO list
	FROM (
		SELECT t FROM _pgtest.user_tables() t WHERE pg_relation_size(t) > 0
		UNION
		SELECT rel::oid FROM _pgtest.seeds
	) w(t);
	IF list IS NOT NULL THEN
		EXECUTE 'TRUNCATE ' || list || ' CASCADE';
	END IF;

	FOR s IN SELECT * FROM _pgtest.seeds LOOP
		IF s.cols = '' THEN
			EXECUTE format('INSERT INTO %s OVERRIDING SYSTEM VALUE SELECT FROM %s', s.rel, s.copy);
		ELSE
			EXECUTE format('INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s',
				s.rel, s.cols, s.cols, s.copy);
		END IF;
	END LOOP;

	-- A sequence whose saved is_called is false is always set back. The
	-- comparison cannot tell it untouched from advanced exactly once, since
	-- one nextval sets last_value to the saved value with is_called true.
	-- Nor can it tell it from a setval(seq, n, false), after which
	-- pg_sequence_last_value is null as before.
	PERFORM setval(q.seq, q.last_value, q.is_called)
	FROM _pgtest.seqs q
	WHERE NOT q.is_called
	   OR pg_sequence_last_value(q.seq) IS DISTINCT FROM q.last_value;

	RETURN QUERY SELECT true, '';
END
$$;
