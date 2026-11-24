-- Builtin-only guard for clusters that mix kinds of machines (D48).
-- Installed by pg.InstallBuiltinOnly in every database (template0 and
-- template1 included); idempotent. @guard@ is replaced by the Go code.
--
-- Two event triggers:
--   @guard@_refuse_collation (ddl_command_start, CREATE COLLATION):
--     refuses every new collation, so no ICU or operating-system sort
--     rules can come back after they were removed.
--   @guard@_check_ddl (ddl_command_end, every command): logs the command
--     (the guards re-check after any change) and refuses it if the object
--     it made or changed uses a collation that is not built in (the
--     predefined ICU "unicode" collation cannot be dropped, so its use is
--     refused here).
-- CREATE DATABASE cannot be seen by event triggers; the guards check
-- every database's provider instead.

CREATE SCHEMA IF NOT EXISTS @guard@;
REVOKE ALL ON SCHEMA @guard@ FROM PUBLIC;
COMMENT ON SCHEMA @guard@ IS 'Kept by coyote: only built-in text sorting may be used, because this cluster mixes kinds of machines. Do not change.';

CREATE TABLE IF NOT EXISTS @guard@.ddl_log (
	id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	at timestamptz NOT NULL DEFAULT now(),
	tag text NOT NULL
);
REVOKE ALL ON @guard@.ddl_log FROM PUBLIC;

CREATE OR REPLACE FUNCTION @guard@.unportable_collations() RETURNS oid[]
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
	SELECT coalesce(array_agg(c.oid), '{}') FROM pg_collation c
	WHERE (c.collprovider = 'i' OR (c.collprovider = 'c' AND NOT (coalesce(c.collcollate, '') IN ('C', 'POSIX') AND coalesce(c.collctype, '') IN ('C', 'POSIX'))))
	  AND c.collencoding IN (-1, pg_char_to_encoding(current_setting('server_encoding')))
$f$;

-- D49: functions whose result can differ in the last bit between builds
-- (fused multiply-add, the maths library): every function taking or
-- giving float4, float8 or a geometric type, except what B-tree and hash
-- indexes on floats use (comparing raw IEEE values and hashing their bits,
-- the same on every machine; the probe's float_compare and hash groups
-- check it), sign changes, abs, float4 <-> float8 and integer -> float
-- (one correctly rounded IEEE conversion each, nothing to fuse). numeric
-- -> float stays out (it goes through text parsing).
CREATE OR REPLACE FUNCTION @guard@.float_math_funcs() RETURNS oid[]
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
	WITH ft AS (
		SELECT array_agg(t.oid) AS ft FROM pg_type t
		WHERE t.typnamespace = 'pg_catalog'::regnamespace AND t.typname IN ('float4', 'float8', 'point', 'lseg', 'path', 'box', 'polygon', 'line', 'circle')
	), fam AS (
		SELECT f.oid FROM pg_opfamily f JOIN pg_am m ON m.oid = f.opfmethod
		WHERE m.amname IN ('btree', 'hash') AND f.opfname = 'float_ops' AND f.opfnamespace = 'pg_catalog'::regnamespace
	), ok AS (
		SELECT p.amproc::oid AS oid FROM pg_amproc p WHERE p.amprocfamily IN (SELECT oid FROM fam)
		UNION SELECT o.oprcode::oid FROM pg_amop a JOIN pg_operator o ON o.oid = a.amopopr WHERE a.amopfamily IN (SELECT oid FROM fam)
		UNION SELECT p.oid FROM pg_proc p WHERE p.pronamespace = 'pg_catalog'::regnamespace
			AND p.proname IN ('float4abs', 'float8abs', 'float4um', 'float8um', 'float4up', 'float8up', 'ftod', 'dtof', 'float4', 'float8')
			AND p.pronargs = 1 AND p.proargtypes[0] IN ('float4'::regtype, 'float8'::regtype, 'int2'::regtype, 'int4'::regtype, 'int8'::regtype)
		-- <> is not in the B-tree family but is the same exact comparison.
		UNION SELECT p.oid FROM pg_proc p WHERE p.pronamespace = 'pg_catalog'::regnamespace
			AND p.proname IN ('float4ne', 'float8ne', 'float48ne', 'float84ne')
	)
	SELECT coalesce(array_agg(p.oid), '{}') FROM pg_proc p, ft
	WHERE (p.prorettype = ANY (ft.ft) OR p.proargtypes::oid[] && ft.ft) AND p.oid NOT IN (SELECT oid FROM ok)
$f$;

-- The functions a stored expression tree calls (FuncExpr funcid, OpExpr
-- and friends opfuncid, and opno through the operator's function).
CREATE OR REPLACE FUNCTION @guard@.tree_funcs(tree text) RETURNS SETOF oid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
	SELECT m[2]::oid FROM regexp_matches(coalesce(tree, ''), ':(funcid|opfuncid) ([0-9]+)', 'g') m
	UNION
	SELECT o.oprcode::oid FROM regexp_matches(coalesce(tree, ''), ':opno ([0-9]+)', 'g') m JOIN pg_operator o ON o.oid = m[1]::oid
$f$;

-- D49: structures that recompute float or geometric maths and compare it
-- with stored results, for relations rels and types typs (NULL, NULL: the
-- whole database): expression and partial indexes, indexes with a
-- geometric operator class (GiST, SP-GiST, BRIN), CHECK and exclusion
-- constraints (domains too), stored generated columns, partition keys,
-- and indexes of an extension's index method whose support functions are
-- float maths (pgvector's HNSW and IVFFlat: their distances are summed in
-- an order that depends on the machine, D53).
-- Plain B-tree, hash and BRIN on float columns compare raw values and are
-- not listed.
-- Temporary tables live in one session and are never replicated as data.
CREATE OR REPLACE FUNCTION @guard@.is_temp(rel oid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
	SELECT EXISTS (SELECT 1 FROM pg_class k WHERE k.oid = rel AND k.relpersistence = 't')
$f$;

CREATE OR REPLACE FUNCTION @guard@.float_math_findings(rels oid[], typs oid[]) RETURNS SETOF text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
	bad oid[] := @guard@.float_math_funcs();
	geo oid[] := ARRAY['point', 'lseg', 'path', 'box', 'polygon', 'line', 'circle']::regtype[]::oid[];
	everything boolean := rels IS NULL AND typs IS NULL;
BEGIN
	RETURN QUERY
	SELECT format('index %s', i.indexrelid::regclass) FROM pg_index i
	WHERE NOT @guard@.is_temp(i.indrelid) AND (everything AND i.indexrelid >= 16384 OR i.indrelid = ANY (rels) OR i.indexrelid = ANY (rels))
	  AND (EXISTS (SELECT 1 FROM @guard@.tree_funcs(i.indexprs::text) f WHERE f = ANY (bad))
	    OR EXISTS (SELECT 1 FROM @guard@.tree_funcs(i.indpred::text) f WHERE f = ANY (bad))
	    OR EXISTS (SELECT 1 FROM unnest(i.indclass::oid[]) k JOIN pg_opclass c ON c.oid = k WHERE c.opcintype = ANY (geo))
	    -- D53: an index method from an extension (pgvector's hnsw and
	    -- ivfflat) whose support functions are float maths (distances).
	    OR EXISTS (SELECT 1 FROM unnest(i.indclass::oid[]) k JOIN pg_opclass c ON c.oid = k JOIN pg_am m ON m.oid = c.opcmethod
	               JOIN pg_amproc p ON p.amprocfamily = c.opcfamily
	               WHERE m.amname NOT IN ('btree', 'hash', 'brin', 'gin', 'gist', 'spgist') AND p.amproc::oid = ANY (bad)))
	UNION ALL
	SELECT format('constraint %I on %s', c.conname, CASE WHEN c.conrelid <> 0 THEN c.conrelid::regclass::text ELSE c.contypid::regtype::text END) FROM pg_constraint c
	WHERE NOT @guard@.is_temp(c.conrelid) AND (everything AND c.oid >= 16384 OR c.conrelid = ANY (rels) OR c.contypid = ANY (typs)) AND c.contype IN ('c', 'x')
	  AND (EXISTS (SELECT 1 FROM @guard@.tree_funcs(c.conbin::text) f WHERE f = ANY (bad))
	    OR EXISTS (SELECT 1 FROM unnest(c.conexclop) x JOIN pg_operator o ON o.oid = x WHERE o.oprcode::oid = ANY (bad)))
	UNION ALL
	SELECT format('generated column %s.%I', d.adrelid::regclass, a.attname) FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
	WHERE NOT @guard@.is_temp(d.adrelid) AND (everything AND d.adrelid >= 16384 OR d.adrelid = ANY (rels)) AND a.attgenerated = 's'
	  AND EXISTS (SELECT 1 FROM @guard@.tree_funcs(d.adbin::text) f WHERE f = ANY (bad))
	UNION ALL
	SELECT format('partition key of %s', p.partrelid::regclass) FROM pg_partitioned_table p
	WHERE NOT @guard@.is_temp(p.partrelid) AND (everything AND p.partrelid >= 16384 OR p.partrelid = ANY (rels))
	  AND EXISTS (SELECT 1 FROM @guard@.tree_funcs(p.partexprs::text) f WHERE f = ANY (bad));
END
$f$;

CREATE OR REPLACE FUNCTION @guard@.refuse_collation() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
BEGIN
	RAISE EXCEPTION 'This cluster mixes kinds of machines, so new collations cannot be made: only the built-in text sorting works the same on every machine.'
		USING ERRCODE = 'feature_not_supported',
		      HINT = 'Leave out COLLATE (the database default is built in), or use COLLATE pg_c_utf8, "C" or pg_unicode_fast.';
END
$f$;

CREATE OR REPLACE FUNCTION @guard@.check_ddl() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
	bad oid[];
	rx text;
	r record;
	hit text;
	rels oid[];
	typs oid[];
BEGIN
	-- Temporary objects are per session and never replicated as data.
	IF NOT EXISTS (SELECT 1 FROM pg_event_trigger_ddl_commands() d WHERE d.schema_name IS NULL OR d.schema_name !~ '^pg_temp') THEN
		RETURN;
	END IF;
	INSERT INTO @guard@.ddl_log (tag) VALUES (tg_tag);

	-- D49: float or geometric maths that a new main node would recompute.
	SELECT array_agg(DISTINCT x) FILTER (WHERE x IS NOT NULL) INTO rels FROM (
		SELECT d.objid AS x FROM pg_event_trigger_ddl_commands() d WHERE d.classid = 'pg_class'::regclass
		UNION SELECT c.conrelid FROM pg_event_trigger_ddl_commands() d JOIN pg_constraint c ON c.oid = d.objid WHERE d.classid = 'pg_constraint'::regclass AND c.conrelid <> 0
	) s;
	SELECT array_agg(DISTINCT x) FILTER (WHERE x IS NOT NULL) INTO typs FROM (
		SELECT d.objid AS x FROM pg_event_trigger_ddl_commands() d WHERE d.classid = 'pg_type'::regclass
		UNION SELECT c.contypid FROM pg_event_trigger_ddl_commands() d JOIN pg_constraint c ON c.oid = d.objid WHERE d.classid = 'pg_constraint'::regclass AND c.contypid <> 0
	) s;
	IF rels IS NOT NULL OR typs IS NOT NULL THEN
		SELECT string_agg(f, ', ') INTO hit FROM @guard@.float_math_findings(coalesce(rels, '{}'), coalesce(typs, '{}')) f;
		IF hit IS NOT NULL THEN
			RAISE EXCEPTION 'This cluster mixes kinds of machines, so stored structures may not recompute decimal (floating-point) or geometric maths; % would, and such results can differ in the last digit between kinds of machines.', hit
				USING ERRCODE = 'feature_not_supported',
				      HINT = 'Plain indexes on float columns are fine (CREATE INDEX ON t (x)); compare with typed constants (x > float8 ''0.5'', not x > 0.5), use an exact type (numeric, integer) for maths, and do not index geometric types or build vector indexes (HNSW, IVFFlat).';
		END IF;
	END IF;

	bad := @guard@.unportable_collations();
	IF cardinality(bad) = 0 THEN
		RETURN;
	END IF;
	-- Collation fields in stored expression trees: ":inputcollid 963",
	-- ":collOid 963", ":colCollations (o 100 963)" and the like.
	rx := 'coll[a-z]* \(?(o )?([0-9]+ )*(' || array_to_string(bad, '|') || ')\M';
	FOR r IN SELECT * FROM pg_event_trigger_ddl_commands() LOOP
		hit := NULL;
		IF r.classid = 'pg_class'::regclass THEN
			SELECT string_agg(x, ', ') INTO hit FROM (
				SELECT format('column %s.%I', a.attrelid::regclass, a.attname) AS x FROM pg_attribute a
				WHERE a.attrelid = r.objid AND a.attnum > 0 AND NOT a.attisdropped AND a.attcollation = ANY (bad)
				UNION ALL
				SELECT format('index %s', i.indexrelid::regclass) FROM pg_index i
				WHERE (i.indrelid = r.objid OR i.indexrelid = r.objid)
				  AND (i.indcollation::oid[] && bad OR coalesce(i.indexprs::text, '') ~* rx OR coalesce(i.indpred::text, '') ~* rx)
				UNION ALL
				SELECT format('constraint %I on %s', c.conname, r.objid::regclass) FROM pg_constraint c
				WHERE c.conrelid = r.objid AND coalesce(c.conbin::text, '') ~* rx
				UNION ALL
				SELECT format('default of %s', r.objid::regclass) FROM pg_attrdef d
				WHERE d.adrelid = r.objid AND d.adbin::text ~* rx
				UNION ALL
				SELECT format('view or rule on %s', r.objid::regclass) FROM pg_rewrite w
				WHERE w.ev_class = r.objid AND (w.ev_action::text ~* rx OR coalesce(w.ev_qual::text, '') ~* rx)
				UNION ALL
				SELECT format('partition key of %s', r.objid::regclass) FROM pg_partitioned_table p
				WHERE p.partrelid = r.objid AND (p.partcollation::oid[] && bad OR coalesce(p.partexprs::text, '') ~* rx)
				UNION ALL
				SELECT format('statistics %I', s.stxname) FROM pg_statistic_ext s
				WHERE s.stxrelid = r.objid AND coalesce(s.stxexprs::text, '') ~* rx
				UNION ALL
				SELECT format('policy %I on %s', p.polname, r.objid::regclass) FROM pg_policy p
				WHERE p.polrelid = r.objid AND (coalesce(p.polqual::text, '') ~* rx OR coalesce(p.polwithcheck::text, '') ~* rx)
				UNION ALL
				SELECT format('trigger %I on %s', t.tgname, r.objid::regclass) FROM pg_trigger t
				WHERE t.tgrelid = r.objid AND coalesce(t.tgqual::text, '') ~* rx
			) found;
		ELSIF r.classid = 'pg_type'::regclass THEN
			SELECT string_agg(x, ', ') INTO hit FROM (
				SELECT format('type %s', t.oid::regtype) AS x FROM pg_type t
				WHERE t.oid = r.objid AND (t.typcollation = ANY (bad) OR coalesce(t.typdefaultbin::text, '') ~* rx)
				UNION ALL
				SELECT format('check on type %s', r.objid::regtype) FROM pg_constraint c
				WHERE c.contypid = r.objid AND coalesce(c.conbin::text, '') ~* rx
				UNION ALL
				SELECT format('field %I of type %s', a.attname, r.objid::regtype) FROM pg_type t JOIN pg_attribute a ON a.attrelid = t.typrelid
				WHERE t.oid = r.objid AND a.attnum > 0 AND NOT a.attisdropped AND a.attcollation = ANY (bad)
				UNION ALL
				SELECT format('range type %s', r.objid::regtype) FROM pg_range g
				WHERE g.rngtypid = r.objid AND g.rngcollation = ANY (bad)
			) found;
		ELSIF r.classid = 'pg_constraint'::regclass THEN
			SELECT format('constraint %I', c.conname) INTO hit FROM pg_constraint c
			WHERE c.oid = r.objid AND coalesce(c.conbin::text, '') ~* rx;
		ELSIF r.classid = 'pg_proc'::regclass THEN
			SELECT format('function %s', p.oid::regprocedure) INTO hit FROM pg_proc p
			WHERE p.oid = r.objid AND coalesce(p.prosqlbody::text, '') ~* rx;
		ELSIF r.classid = 'pg_rewrite'::regclass THEN
			SELECT format('rule %I', w.rulename) INTO hit FROM pg_rewrite w
			WHERE w.oid = r.objid AND (w.ev_action::text ~* rx OR coalesce(w.ev_qual::text, '') ~* rx);
		ELSIF r.classid = 'pg_policy'::regclass THEN
			SELECT format('policy %I', p.polname) INTO hit FROM pg_policy p
			WHERE p.oid = r.objid AND (coalesce(p.polqual::text, '') ~* rx OR coalesce(p.polwithcheck::text, '') ~* rx);
		ELSIF r.classid = 'pg_trigger'::regclass THEN
			SELECT format('trigger %I', t.tgname) INTO hit FROM pg_trigger t
			WHERE t.oid = r.objid AND coalesce(t.tgqual::text, '') ~* rx;
		ELSIF r.classid = 'pg_statistic_ext'::regclass THEN
			SELECT format('statistics %I', s.stxname) INTO hit FROM pg_statistic_ext s
			WHERE s.oid = r.objid AND coalesce(s.stxexprs::text, '') ~* rx;
		END IF;
		IF hit IS NOT NULL THEN
			RAISE EXCEPTION 'This cluster mixes kinds of machines, so text may only use the built-in sorting; % would use ICU or operating-system sort rules, which differ between machines.', hit
				USING ERRCODE = 'feature_not_supported',
				      HINT = 'Leave out COLLATE (the database default is built in), or use COLLATE pg_c_utf8, "C" or pg_unicode_fast.';
		END IF;
	END LOOP;
END
$f$;

REVOKE ALL ON FUNCTION @guard@.unportable_collations() FROM PUBLIC;
REVOKE ALL ON FUNCTION @guard@.refuse_collation() FROM PUBLIC;
REVOKE ALL ON FUNCTION @guard@.check_ddl() FROM PUBLIC;
REVOKE ALL ON FUNCTION @guard@.float_math_funcs() FROM PUBLIC;
REVOKE ALL ON FUNCTION @guard@.tree_funcs(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION @guard@.is_temp(oid) FROM PUBLIC;
REVOKE ALL ON FUNCTION @guard@.float_math_findings(oid[], oid[]) FROM PUBLIC;

DO $d$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_event_trigger WHERE evtname = '@guard@_refuse_collation') THEN
		CREATE EVENT TRIGGER @guard@_refuse_collation ON ddl_command_start
			WHEN TAG IN ('CREATE COLLATION') EXECUTE FUNCTION @guard@.refuse_collation();
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_event_trigger WHERE evtname = '@guard@_check_ddl') THEN
		CREATE EVENT TRIGGER @guard@_check_ddl ON ddl_command_end EXECUTE FUNCTION @guard@.check_ddl();
	END IF;
END
$d$;
ALTER EVENT TRIGGER @guard@_refuse_collation ENABLE ALWAYS;
ALTER EVENT TRIGGER @guard@_check_ddl ENABLE ALWAYS;
