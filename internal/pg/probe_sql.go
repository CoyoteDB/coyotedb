package pg

import "strings"

// The probe's statements (probe.go). Rules for editing:
//
//   - bump ProbeVersion on any change;
//   - deterministic only: no clock, no random, nothing per-cluster (OIDs,
//     names of the machine, settings other than those probeSession fixes);
//   - every row order fixed by ORDER BY; aggregate large outputs with md5
//     so results stay small;
//   - built-in functions only (no extension is installed when it runs);
//   - read-only, so it runs on a standby.

// probeWords is a sample of text in many scripts and every case-mapping
// oddity we know of: Turkish dotted/dotless i, German sharp s (and its
// capital), Greek final sigma, title-case digraphs, ligatures, combining
// marks (decomposed and composed forms), Cherokee and Georgian (whose case
// rules changed in recent Unicode versions), letter-like symbols (Kelvin,
// Angstrom, micro), full-width forms, characters outside the BMP (Deseret,
// Osage, Adlam, emoji, CJK extension B), right-to-left scripts and
// Indic scripts with combining vowel signs.
const probeWords = `(VALUES
 (1,'apple'),(2,'Äpfel'),(3,'éclair'),(4,'naïve'),(5,'Zürich'),(6,'İstanbul'),(7,'ıssız'),(8,'straße'),
 (9,'ΑΘΗΝΑ'),(10,'αθήνα'),(11,'ΟΔΥΣΣΕΥΣ'),(12,'Москва'),(13,'москва'),(14,'北京'),(15,'東京'),(16,'서울'),
 (17,'القاهرة'),(18,'ירושלים'),(19,'नमस्ते'),(20,'ไทย'),(21,'😀 emoji 👍🏽'),(22,'ǅ ǈ ǋ ǲ'),(23,'ß'),(24,'ẞ'),
 (25,'ﬁ ﬂ ﬀ ﬃ ﬄ ﬅ ﬆ'),(26,'e' || U&'\0301'),(27,'é'),(28,''),(29,' leading and trailing '),(30,'UPPER'),
 (31,'lower'),(32,'MiXeD CaSe'),(33,'z'),(34,'Z'),(35,'a'),(36,'A'),(37,'ŉ ΐ ΰ ǰ'),(38,'Ꭰꭰ ᏸ ᏰᏱ'),
 (39,'ᲐᲑᲒ აბგ'),(40,U&'\212A \212B \00B5 \03BC \017F'),(41,'ＡＢＣ ａｂｃ ０１２'),(42,U&'\+010400\+010428 \+0104B0\+0104D8'),
 (43,U&'\+01E900\+01E922'),(44,U&'\+020000\+02A6D6'),(45,'Ǆ ǆ ǅ'),(46,'Σ σ ς'),(47,'İi Iı'),(48,'Ⅷ ⅷ ⓐ Ⓐ'),
 (49,'ꞵ Ꞵ Ꟁꟁ'),(50,'tab	and' || chr(10) || 'newline'),(51,'0123456789٠١٢३४'),(52,'!"#$%&''()*+,-./:;<=>?@[\]^_{|}~'),
 (53,U&'\00A0\2000\2028\3000'),(54,'Ελληνικά ΆΈΉΊΌΎΏ άέήίόύώ'),(55,'Ŀŀ Ĳĳ ǈ Ǉ'),(56,'ⰀⰁ ⰰⰱ Ⴀ ⴀ'),
 (57,'ﬓ ﬔ ﬕ'),(58,'ǰ̌'),(59,'ᾈ ᾀ ᾼ ῌ'),(60,'Ꙁ ꙁ Ꚁ ꚁ')) AS w(id, s)`

// probeInstants are moments around time-zone rule changes and far from
// today, as timestamptz literals (UTC).
const probeInstants = `(VALUES
 ('1883-11-18 16:59:59+00'),('1883-11-18 17:00:00+00'),('1901-12-13 20:45:52+00'),('1918-03-31 06:59:59+00'),('1918-03-31 07:00:00+00'),
 ('1942-02-09 06:59:59+00'),('1945-09-30 06:00:00+00'),('1970-01-01 00:00:00+00'),('1973-12-29 00:00:00+00'),('1986-04-27 06:59:59+00'),
 ('1990-03-25 00:59:59+00'),('1995-12-31 23:59:59+00'),('2000-02-29 12:00:00+00'),('2005-03-27 00:59:59+00'),('2005-03-27 01:00:00+00'),
 ('2005-10-30 00:59:59+00'),('2005-10-30 01:00:00+00'),('2007-03-11 06:59:59+00'),('2007-03-11 07:00:00+00'),('2007-11-04 05:59:59+00'),
 ('2007-11-04 06:00:00+00'),('2011-12-29 09:59:59+00'),('2011-12-29 10:00:00+00'),('2014-10-25 21:59:59+00'),('2016-12-31 23:59:59+00'),
 ('2022-09-11 00:00:00+00'),('2024-03-10 06:59:59+00'),('2024-03-10 07:00:00+00'),('2024-03-31 00:59:59+00'),('2024-03-31 01:00:00+00'),
 ('2024-10-27 00:59:59+00'),('2024-10-27 01:00:00+00'),('2024-11-03 05:59:59+00'),('2024-11-03 06:00:00+00'),('2026-04-05 15:30:00+00'),
 ('2037-12-31 23:59:59+00'),('2038-01-19 03:14:07+00'),('2038-01-19 03:14:08+00'),('2050-07-01 12:00:00+00'),('2100-03-28 00:59:59+00'),
 ('2100-03-28 01:00:00+00'),('2400-10-31 01:00:00+00'),('9999-12-31 23:59:59.999999+00'),('0001-01-01 00:00:00+00'),('0044-03-15 12:00:00+00 BC'),
 ('4713-11-24 00:00:00+00 BC'),('200000-01-01 00:00:00+00'),('infinity'),('-infinity')) AS i(t)`

// probeZones are zones with unusual rules: half and quarter hours,
// 30-minute DST, date-line jumps, permanent changes, negative DST,
// southern hemisphere, POSIX-style and fixed-offset names.
var probeZones = []string{
	"UTC", "America/New_York", "America/Los_Angeles", "America/St_Johns", "America/Sao_Paulo", "America/Santiago",
	"America/Havana", "America/Caracas", "America/Godthab", "Europe/London", "Europe/Dublin", "Europe/Berlin",
	"Europe/Moscow", "Europe/Istanbul", "Europe/Lisbon", "Africa/Casablanca", "Africa/Cairo", "Asia/Kolkata",
	"Asia/Kathmandu", "Asia/Tehran", "Asia/Shanghai", "Asia/Tokyo", "Asia/Gaza", "Asia/Pyongyang",
	"Australia/Lord_Howe", "Australia/Adelaide", "Australia/Eucla", "Pacific/Chatham", "Pacific/Apia",
	"Pacific/Kiritimati", "Pacific/Marquesas", "Antarctica/Troll", "EST5EDT", "Etc/GMT+12", "Etc/GMT-14",
}

func zoneList(zs []string) string {
	q := make([]string, len(zs))
	for i, z := range zs {
		q[i] = "('" + z + "')"
	}
	return "(VALUES " + strings.Join(q, ",") + ") AS z(z)"
}

// tzStatement converts every probe instant to local time in each zone,
// and back from local time to UTC.
func tzStatement(zs []string) string {
	return `SELECT z.z, md5(string_agg(
		(i.t::timestamptz AT TIME ZONE z.z)::text || '|' ||
		coalesce(((i.t::timestamptz AT TIME ZONE z.z) AT TIME ZONE z.z)::text, '') || '|' ||
		CASE WHEN isfinite(i.t::timestamptz) THEN to_char(i.t::timestamptz AT TIME ZONE z.z, 'YYYY-MM-DD HH24:MI:SS') ELSE '' END,
		';' ORDER BY i.t::timestamptz)) FROM ` + probeInstants + `, ` + zoneList(zs) + ` GROUP BY z.z ORDER BY z.z`
}

// float8 bit patterns (float8send) are compared, not just text, so a
// difference in the last bit is always seen.
const f8bits = `encode(float8send(%s), 'hex')`

func bits(expr string) string { return strings.ReplaceAll(f8bits, "%s", expr) }

// libmStatement: one maths-library function over a range of inputs, as
// exact bit patterns.
func libmStatement(name, expr, from, to, step string) ProbeStatement {
	return ProbeStatement{Name: name, What: name + "()", SQL: `SELECT md5(string_agg(coalesce(` + bits(expr) + `, 'null'), ',' ORDER BY x)), count(*) FROM generate_series(` + from + `::numeric, ` + to + `::numeric, ` + step + `::numeric) AS g(n), LATERAL (SELECT n::float8 AS x) v`}
}

var probeGroups = []ProbeGroup{
	{
		// D49: informational. Physical replication ships stored bits, so a
		// last-bit difference (fused multiply-add in one build, the maths
		// library) matters only where a new main node recomputes float math
		// and compares it with stored results; the builtin-only guard
		// refuses those structures (builtinonly.sql) instead. What indexes
		// rely on (comparing and hashing) stays must-match: float_compare,
		// hash.
		Name: "float", What: "working with decimal (floating-point) numbers (reported only)", MustMatch: false,
		Statements: []ProbeStatement{
			{Name: "f8_out_edges", What: "printing special float8 values", SQL: `SELECT string_agg(v || '=' || v::float8::text || '/' || ` + bits("v::float8") + `, ' ' ORDER BY o) FROM (VALUES
				(1,'0'),(2,'-0'),(3,'4.9e-324'),(4,'2.2250738585072009e-308'),(5,'2.2250738585072014e-308'),(6,'1.7976931348623157e308'),
				(7,'Infinity'),(8,'-Infinity'),(9,'NaN'),(10,'0.1'),(11,'0.2'),(12,'0.3'),(13,'1e23'),(14,'9007199254740993'),
				(15,'123456789012345678'),(16,'1e-310'),(17,'5e-324'),(18,'2.4703282292062328e-324'),(19,'0.30000000000000004'),
				(20,'1.00000000000000011102230246251565404236316680908203125'),(21,'1.000000000000000111022302462515654042363166809082031251'),
				(22,'7.0e-10'),(23,'-1.5e300'),(24,'3.14159265358979323846264338327950288'),(25,'2.718281828459045'),(26,'1e15'),(27,'1e16'),(28,'1e17'),
				(29,'123.456e-7'),(30,'  42  '),(31,'+inf'),(32,'-nan')) t(o, v)`},
			{Name: "f4_out_edges", What: "printing special float4 values", SQL: `SELECT string_agg(v || '=' || v::float4::text || '/' || encode(float4send(v::float4), 'hex'), ' ' ORDER BY o) FROM (VALUES
				(1,'0'),(2,'-0'),(3,'1.4e-45'),(4,'1.17549435e-38'),(5,'3.4028235e38'),(6,'Infinity'),(7,'NaN'),(8,'0.1'),(9,'16777217'),
				(10,'1.00000006'),(11,'3.14159265358979'),(12,'1e-40'),(13,'7.0e-10'),(14,'-123.456')) t(o, v)`},
			{Name: "f8_series_text", What: "printing many float8 values", SQL: `SELECT md5(string_agg((g::float8/7)::text || ',' || (1/g::float8)::text || ',' || (g*0.1::float8)::text || ',' || (g::float8*1e300)::text || ',' || (g::float8*1e-300)::text || ',' || (g::float8/3e10)::text, '|' ORDER BY g)) FROM generate_series(1, 5000) g`},
			{Name: "f4_series_text", What: "printing many float4 values", SQL: `SELECT md5(string_agg((g::float4/7)::text || ',' || (1/g::float4)::text || ',' || (g*0.1::float4)::text || ',' || (g::float8/7)::float4::text, '|' ORDER BY g)) FROM generate_series(1, 5000) g`},
			{Name: "f8_parse", What: "reading float8 values from text", SQL: `SELECT md5(string_agg(` + bits("(g::text || '.' || lpad((g*7919 % 100000)::text, 5, '0') || 'e' || (g % 600 - 300)::text)::float8") + `, ',' ORDER BY g)) FROM generate_series(1, 5000) g`},
			{Name: "f8_parse_halfway", What: "rounding float8 text exactly half-way", SQL: `SELECT string_agg(` + bits("v::float8") + `, ',' ORDER BY v) FROM (VALUES ('9007199254740993'),('9007199254740995'),('2.00000000000000011102230246251565404236316680908203125'),('1.0000000000000002220446049250313080847263336181640625'),('4.9406564584124654e-324'),('2.4703282292062328e-324'),('1.7976931348623158e308')) t(v)`},
			{Name: "f8_arith", What: "adding, multiplying and dividing float8", SQL: `SELECT md5(string_agg(` + bits("a+b") + ` || ` + bits("a-b") + ` || ` + bits("a*b") + ` || ` + bits("a/b") + ` || ` + bits("a*a*a/b") + `, ',' ORDER BY g)) FROM (SELECT g, g::float8/3 AS a, (g+1)::float8/7e-3 AS b FROM generate_series(1, 5000) g) s`},
			{Name: "f8_round", What: "rounding float8", SQL: `SELECT md5(string_agg(` + bits("floor(x)") + ` || ` + bits("ceil(x)") + ` || ` + bits("round(x)") + ` || ` + bits("trunc(x)") + ` || ` + bits("abs(x)") + ` || sign(x)::text || x::int8::text || ` + bits("x::float4::float8") + `, ',' ORDER BY g)) FROM (SELECT g, (g - 2500)::float8 / 4 + g::float8/1e9 AS x FROM generate_series(1, 5000) g) s`},
			{Name: "f8_round_half", What: "rounding float8 exactly half-way", SQL: `SELECT string_agg(x::text || '>' || round(x)::text || '/' || x::int4::text || '/' || x::int8::text || '/' || x::int2::text, ' ' ORDER BY x) FROM (VALUES (-2.5::float8),(-1.5),(-0.5),(0.5),(1.5),(2.5),(3.5),(4.5),(1e15+0.5)) t(x) WHERE x::numeric < 32767`},
			{Name: "f8_sqrt", What: "square roots of float8", SQL: `SELECT md5(string_agg(` + bits("sqrt(g::float8)") + ` || ` + bits("sqrt(g::float8/1e300)") + ` || ` + bits("sqrt(g::float8*1e300)") + `, ',' ORDER BY g)) FROM generate_series(0, 5000) g`},
			{Name: "f8_power_exact", What: "powers of float8 that are exact", SQL: `SELECT md5(string_agg(` + bits("power(b::float8, e::float8)") + `, ',' ORDER BY b, e)) FROM (SELECT 2 AS b, e FROM generate_series(-1074, 1023) e UNION ALL SELECT 3, e FROM generate_series(0, 33) e UNION ALL SELECT -2, e FROM generate_series(0, 60) e UNION ALL SELECT 10, e FROM generate_series(0, 22) e UNION ALL SELECT 0.5, e FROM generate_series(0, 60) e) s`},
			{Name: "f8_to_numeric", What: "turning float8 into exact numbers", SQL: `SELECT md5(string_agg((g::float8/7)::numeric::text || ',' || (g::float8*1e-20/3)::numeric::text || ',' || (g::float4/7)::numeric::text || ',' || ((g::numeric/7)::float8)::text || ',' || ((g::numeric/7)::float4)::text, '|' ORDER BY g)) FROM generate_series(1, 3000) g`},
			{Name: "f8_to_char", What: "formatting float8 with to_char", SQL: `SELECT md5(string_agg(to_char(g::float8/7, 'FM999990.999999999') || ',' || to_char(g::float8*1e10/3, '9.999999EEEE') || ',' || to_char(g::float4/3, '99999.9999'), '|' ORDER BY g)) FROM generate_series(1, 3000) g`},
			{Name: "f8_legacy_digits0", What: "printing float8 with extra_float_digits = 0", Setup: []string{"SET LOCAL extra_float_digits = 0"}, SQL: `SELECT md5(string_agg((g::float8/7)::text || ',' || (g::float4/7)::text || ',' || (g::float8*1e300/7)::text, '|' ORDER BY g)) FROM generate_series(1, 3000) g`},
			{Name: "f8_legacy_digits_neg", What: "printing float8 with extra_float_digits = -4", Setup: []string{"SET LOCAL extra_float_digits = -4"}, SQL: `SELECT md5(string_agg((g::float8/7)::text || ',' || (g::float4/7)::text, '|' ORDER BY g)) FROM generate_series(1, 3000) g`},
			{Name: "f8_degrees_radians", What: "degrees() and radians()", SQL: `SELECT md5(string_agg(` + bits("degrees(g::float8/100)") + ` || ` + bits("radians(g::float8)") + `, ',' ORDER BY g)) FROM generate_series(-2000, 2000) g`},
			{Name: "f8_aggregates", What: "float8 sums, averages and deviations", SQL: `SELECT ` + bits("sum(x)") + `, ` + bits("avg(x)") + `, ` + bits("stddev_samp(x)") + `, ` + bits("var_pop(x)") + `, ` + bits("corr(x, y)") + `, ` + bits("regr_slope(y, x)") + `, ` + bits("regr_intercept(y, x)") + `, ` + bits("covar_samp(x, y)") + ` FROM (SELECT g::float8/7 AS x, (g*g % 1013)::float8/3 AS y FROM generate_series(1, 20000) g) s`},
			{Name: "f4_aggregates", What: "float4 sums and averages", SQL: `SELECT sum(x)::text, avg(x)::text, stddev(x)::text FROM (SELECT g::float4/7 AS x FROM generate_series(1, 20000) g) s`},
			{Name: "f8_width_bucket", What: "width_bucket on float8", SQL: `SELECT md5(string_agg(width_bucket(g::float8/7, -100, 1000, 37)::text || width_bucket(g::float8, 0.0::float8, 5000.0::float8, 3)::text, ',' ORDER BY g)) FROM generate_series(-1000, 6000) g`},
			{Name: "f8_errors", What: "float8 overflow", SQL: `SELECT '1e308'::float8 * 10`},
			{Name: "f8_errors_underflow", What: "float8 underflow", SQL: `SELECT '1e-308'::float8 * '1e-308'::float8`},
			{Name: "f8_errors_parse", What: "reading an out-of-range float8", SQL: `SELECT '1e400'::float8`},
		},
	},
	{
		// D49: what B-tree, hash and BRIN indexes on float columns rely on:
		// comparing raw IEEE values. Must match.
		Name: "float_compare", What: "comparing and sorting decimal (floating-point) numbers", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "f8_sort", What: "sorting float8 including NaN, infinity and -0", SQL: `SELECT string_agg(x::text, ' ' ORDER BY x, x::text), bool_and(('-0'::float8 = '0'::float8) AND ('NaN'::float8 = 'NaN'::float8) AND ('NaN'::float8 > 'Infinity'::float8)) FROM (VALUES ('NaN'::float8),('Infinity'),('-Infinity'),('-0'),('0'),('1e-320'),('-1e-320'),('1'),('-1'),('NaN')) t(x)`},
			{Name: "f48_compare", What: "comparing float4 with float8", SQL: `SELECT md5(string_agg(concat((a < b)::int, (a = b)::int, (a > b)::int, btfloat48cmp(a, b), btfloat84cmp(b, a), btfloat8cmp(b, b::float4::float8)), '' ORDER BY g)) FROM (SELECT g, (g::float4 / 7::float4) AS a, (g::float8 / 7) AS b FROM generate_series(-3000, 3000) g) s`},
		},
	},
	{
		// D49: informational (one build fuses multiply-add in distances).
		// Indexes on geometric types are refused in mixed clusters.
		Name: "geometry", What: "points, boxes and paths (reported only)", MustMatch: false,
		Statements: []ProbeStatement{
			{Name: "geometry", What: "points, boxes and paths (exact operations only)", SQL: `SELECT md5(string_agg(p::text || (p <-> point(0, 0))::text || box(p, point(g, -g))::text || (box(p, point(g, -g)) && box '((0,0),(10,10))')::int::text || area(box(p, point(g, -g)))::text || center(box(p, point(g, -g)))::text || (p + point(1.5, -2.25))::text || (p * point(2, 0))::text || lseg(p, point(0, 0))::text || length(lseg(p, point(0, 0)))::text || (point(g, 0) <@ circle '((0,0),100)')::int::text, '|' ORDER BY g)) FROM (SELECT g, point(g / 7.0, g % 13 - 6) AS p FROM generate_series(1, 2000) g) s`},
		},
	},
	{
		Name: "libm", What: "using the system maths library (reported only)", MustMatch: false,
		Statements: []ProbeStatement{
			libmStatement("exp", "exp(x/100)", "-70000", "70000", "37"),
			libmStatement("ln", "ln(abs(x)+0.001)", "0", "100000", "31"),
			libmStatement("log10", "log10(abs(x)+0.001)", "0", "100000", "31"),
			libmStatement("power_fraction", "power(abs(x)/100+0.01, 0.37)", "0", "100000", "31"),
			libmStatement("power_large", "power(1.0000001::float8, x)", "-100000", "100000", "61"),
			libmStatement("sin", "sin(x/100)", "-100000", "100000", "41"),
			libmStatement("cos", "cos(x/100)", "-100000", "100000", "41"),
			libmStatement("tan", "tan(x/100)", "-100000", "100000", "41"),
			libmStatement("cot", "cot(x/100+0.001)", "-100000", "100000", "41"),
			libmStatement("asin", "asin(x/10000)", "-10000", "10000", "7"),
			libmStatement("acos", "acos(x/10000)", "-10000", "10000", "7"),
			libmStatement("atan", "atan(x/100)", "-100000", "100000", "41"),
			libmStatement("atan2", "atan2(x/100, 3)", "-100000", "100000", "41"),
			libmStatement("sind", "sind(x/10)", "-10000", "10000", "7"),
			libmStatement("cosd", "cosd(x/10)", "-10000", "10000", "7"),
			libmStatement("tand", "tand(x/10)", "-10000", "10000", "7"),
			libmStatement("cotd", "cotd(x/10+0.01)", "-10000", "10000", "7"),
			libmStatement("asind", "asind(x/10000)", "-10000", "10000", "7"),
			libmStatement("acosd", "acosd(x/10000)", "-10000", "10000", "7"),
			libmStatement("atand", "atand(x/100)", "-100000", "100000", "41"),
			libmStatement("atan2d", "atan2d(x/100, 3)", "-100000", "100000", "41"),
			libmStatement("sinh", "sinh(x/1000)", "-700000", "700000", "301"),
			libmStatement("cosh", "cosh(x/1000)", "-700000", "700000", "301"),
			libmStatement("tanh", "tanh(x/1000)", "-30000", "30000", "13"),
			libmStatement("asinh", "asinh(x/100)", "-100000", "100000", "41"),
			libmStatement("acosh", "acosh(abs(x)/100+1)", "0", "100000", "31"),
			libmStatement("atanh", "atanh(x/10001)", "-10000", "10000", "7"),
			libmStatement("cbrt", "cbrt(x/7)", "-100000", "100000", "41"),
			libmStatement("gamma", "gamma(x/100+0.001)", "-1000", "17000", "7"),
			libmStatement("lgamma", "lgamma(x/100+0.001)", "-10000", "100000", "41"),
			libmStatement("erf", "erf(x/1000)", "-6000", "6000", "3"),
			libmStatement("erfc", "erfc(x/1000)", "-6000", "27000", "7"),
			{Name: "ts_rank", What: "text-search ranking", SQL: `SELECT md5(string_agg(ts_rank(to_tsvector('english', s), to_tsquery('english', 'fox | dog'))::text || ts_rank_cd(to_tsvector('english', s), to_tsquery('english', 'fox & dog'), 1|4|32)::text, ',' ORDER BY g)) FROM (SELECT g, repeat('the quick brown fox jumps over the lazy dog ', g % 7 + 1) || repeat('fox ', g % 5) AS s FROM generate_series(1, 500) g) t`},
		},
	},
	{
		Name: "numeric", What: "working with exact (numeric) numbers", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "arith", What: "numeric arithmetic", SQL: `SELECT md5(string_agg((n*n)::text || ',' || (n/7)::text || ',' || (n % 7)::text || ',' || div(n, 7)::text || ',' || (n + 0.000000001)::text || ',' || round(n, 3)::text || ',' || trunc(n, -2)::text || ',' || scale(n/7)::text || ',' || min_scale(n*10)::text || ',' || trim_scale(n*100)::text, '|' ORDER BY g)) FROM (SELECT g, (g * 12345.6789)::numeric - 1000000 AS n FROM generate_series(1, 5000) g) s`},
			{Name: "functions", What: "numeric roots, logarithms and powers", SQL: `SELECT md5(string_agg(sqrt(abs(n))::text || ',' || ln(abs(n) + 1)::text || ',' || exp(n / 1000000)::text || ',' || power(abs(n) / 1000, 3)::text || ',' || power(1.0001, g)::text || ',' || log(10, abs(n) + 1)::text || ',' || power(2::numeric, (g % 100 - 50) / 10.0)::text, '|' ORDER BY g)) FROM (SELECT g, (g * 12345.6789)::numeric - 1000000 AS n FROM generate_series(1, 2000) g) s`},
			{Name: "big", What: "very large and very small numeric values", SQL: `SELECT md5(power(2::numeric, 3000)::text), md5(factorial(300)::text), (1::numeric / 3)::text, ('1e-1000'::numeric * 3)::text, 'NaN'::numeric::text, ('Infinity'::numeric - 1)::text, (-'Infinity'::numeric)::text, gcd(1071::numeric * 10::numeric ^ 40, 462 * 10::numeric ^ 40)::text, lcm(12345678901234567890, 98765432109876543210)::text, (10::numeric ^ 131071 > 0)::text`},
			{Name: "casts", What: "turning numeric into whole numbers", SQL: `SELECT string_agg(x::text || '>' || x::int8::text || '/' || x::int4::text || '/' || x::int2::text || '/' || round(x)::text || '/' || round(x, 0)::text || '/' || ceil(x)::text || '/' || floor(x)::text, ' ' ORDER BY x) FROM (VALUES (-2.5::numeric),(-1.5),(-0.5),(0.5),(1.5),(2.5),(32766.5),(-32767.5)) t(x)`},
			{Name: "text_io", What: "reading and printing numeric", SQL: `SELECT string_agg(v || '=' || v::numeric::text || '/' || v::numeric(50,5)::text, ' ' ORDER BY v) FROM (VALUES ('0'),('-0'),('0.000'),('1e3'),('1.5e-3'),('  12.50  '),('0x1F'),('0o17'),('0b101'),('1_000_000.000_1'),('-123456789012345678901234567890.123456789')) t(v)`},
			{Name: "sort", What: "sorting numeric including NaN and infinity", SQL: `SELECT string_agg(x::text, ' ' ORDER BY x, x::text) FROM (VALUES ('NaN'::numeric),('Infinity'),('-Infinity'),('0'),('0.00'),('-0.001'),('1e-100'),('1.0'),('1'),('1.00')) t(x)`},
			{Name: "aggregates", What: "numeric sums and averages", SQL: `SELECT sum(n)::text, avg(n)::text, stddev(n)::text, variance(n)::text, sum(n::float8)::numeric(30,5)::text FROM (SELECT (g * 1.000001)::numeric AS n FROM generate_series(1, 50000) g) s`},
			{Name: "overflow", What: "numeric overflow", SQL: `SELECT 99999.5::numeric(5,0)`},
		},
	},
	{
		Name: "integer", What: "working with whole numbers", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "ops", What: "whole-number arithmetic and bit operations", SQL: `SELECT md5(string_agg((a / b)::text || ',' || (a % b)::text || ',' || (a::int8 * b)::text || ',' || (a << (g % 31))::text || ',' || (a >> (g % 31))::text || ',' || (a # b)::text || ',' || (a & b)::text || ',' || (~a)::text || ',' || abs(a)::text || ',' || gcd(a, b)::text || ',' || (a::int2 / 3)::text, '|' ORDER BY g)) FROM (SELECT g, (g * 7919 % 65521 - 32760) AS a, (CASE WHEN g % 2 = 0 THEN -1 ELSE 1 END) * (g % 97 + 1) AS b FROM generate_series(1, 5000) g) s`},
			{Name: "edges", What: "whole-number limits", SQL: `SELECT (-2147483648)::int4 / 2, (-2147483648)::int4 % -1, (-9223372036854775808)::int8 % -1, (-32768)::int2 % -1::int2, 2147483647::int8 + 1, 9223372036854775807::int8::numeric + 1, (-9223372036854775808)::int8 >> 63, 1::int8 << 63, 'x7fffffffffffffff'::bit(64)::int8, (-1)::int4::bit(32)::text`},
			{Name: "parse", What: "reading whole numbers from text", SQL: `SELECT string_agg(v || '=' || v::int8::text, ' ' ORDER BY v) FROM (VALUES ('0'),('-0'),('  42 '),('+7'),('0x7FFFFFFFFFFFFFFF'),('-0x8000000000000000'),('0o777'),('0b1111'),('1_000_000'),('-9223372036854775808')) t(v)`},
			{Name: "ovf_int2_add", What: "int2 overflow", SQL: `SELECT 32767::int2 + 1::int2`},
			{Name: "ovf_int4_add", What: "int4 overflow", SQL: `SELECT 2147483647::int4 + 1`},
			{Name: "ovf_int4_div", What: "int4 minimum divided by -1", SQL: `SELECT (-2147483648)::int4 / -1`},
			{Name: "ovf_int4_abs", What: "absolute value of the int4 minimum", SQL: `SELECT abs((-2147483648)::int4)`},
			{Name: "ovf_int4_mul", What: "int4 multiplication overflow", SQL: `SELECT 65536::int4 * 32768::int4`},
			{Name: "ovf_int8_add", What: "int8 overflow", SQL: `SELECT 9223372036854775807::int8 + 1`},
			{Name: "ovf_int8_div", What: "int8 minimum divided by -1", SQL: `SELECT (-9223372036854775808)::int8 / -1`},
			{Name: "ovf_int8_mul", What: "int8 multiplication overflow", SQL: `SELECT 4294967296::int8 * 2147483648::int8`},
			{Name: "ovf_parse", What: "reading an out-of-range int4", SQL: `SELECT '2147483648'::int4`},
			{Name: "ovf_div_zero", What: "dividing by zero", SQL: `SELECT 1 / 0`},
			{Name: "ovf_f8_int4", What: "turning a too-large float8 into int4", SQL: `SELECT 2147483647.5::float8::int4`},
		},
	},
	{
		Name: "hash", What: "computing hash values (used by hash indexes and hash partitions)", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "text", What: "hashes of text", SQL: `SELECT md5(string_agg(hashtext(s)::text || ',' || hashtextextended(s, 0)::text || ',' || hashtextextended(s, 8816678312871386365)::text || ',' || hashtext(s COLLATE "C")::text || ',' || hashtext(s COLLATE pg_c_utf8)::text || ',' || hashtext(md5(s))::text || ',' || hashbpchar(s::char(20))::text || ',' || hashname(s::name)::text, '|' ORDER BY id)) FROM ` + probeWords},
			{Name: "text_many", What: "hashes of many strings", SQL: `SELECT md5(string_agg(hashtextextended(repeat(md5(g::text), g % 9), g)::text, ',' ORDER BY g)) FROM generate_series(1, 5000) g`},
			{Name: "int", What: "hashes of whole numbers", SQL: `SELECT md5(string_agg(hashint2((g % 32767)::int2)::text || ',' || hashint4(g * 7919)::text || ',' || hashint8(g::int8 * 1000000007)::text || ',' || hashint8extended(g::int8 * -97, 8816678312871386365)::text || ',' || hashint4extended(g, 42)::text || ',' || hashoid(g::oid)::text || ',' || hashchar(chr(g % 127 + 1)::"char")::text, '|' ORDER BY g)) FROM generate_series(1, 5000) g`},
			{Name: "int_edges", What: "hashes of whole-number limits", SQL: `SELECT hashint8(-9223372036854775808), hashint8(9223372036854775807), hashint4(-2147483648), hashint4(0), hashint8(0), hashint8(-1), hashint4(-1), hashint8extended(-1, -1), hashint2((-32768)::int2)`},
			{Name: "float", What: "hashes of decimal numbers", SQL: `SELECT md5(string_agg(hashfloat8(g::float8 / 7)::text || ',' || hashfloat4(g::float4 / 7::float4)::text || ',' || hashfloat8extended(g::float8 * 1e300, 8816678312871386365)::text, '|' ORDER BY g)) FROM generate_series(1, 5000) g`},
			{Name: "float_edges", What: "hashes of special decimal values", SQL: `SELECT string_agg(hashfloat8(x)::text || '/' || hashfloat4(x::float4)::text || '/' || hashfloat8extended(x, 1)::text, ' ' ORDER BY o) FROM (VALUES (1, 'NaN'::float8),(2,'-NaN'),(3,'0'),(4,'-0'),(5,'Infinity'),(6,'-Infinity'),(7,'1e-30')) t(o, x)`},
			{Name: "numeric", What: "hashes of exact numbers", SQL: `SELECT md5(string_agg(hash_numeric(n)::text || ',' || hash_numeric(n * 1.000)::text || ',' || hash_numeric_extended(n, 8816678312871386365)::text, '|' ORDER BY g)) FROM (SELECT g, (g * 123.4567)::numeric - 100000 AS n FROM generate_series(1, 5000) g UNION ALL SELECT 0, 'NaN' UNION ALL SELECT -1, 'Infinity' UNION ALL SELECT -2, '-0.000') s`},
			{Name: "uuid", What: "hashes of uuids", SQL: `SELECT md5(string_agg(uuid_hash(md5(g::text)::uuid)::text || ',' || uuid_hash_extended(md5(g::text)::uuid, 8816678312871386365)::text, '|' ORDER BY g)) FROM generate_series(1, 5000) g`},
			{Name: "time", What: "hashes of dates and times", SQL: `SELECT md5(string_agg(timestamp_hash(t)::text || ',' || timestamp_hash_extended(t, 8816678312871386365)::text || ',' || hashint4((t::date - '2000-01-01'::date))::text || ',' || interval_hash(t - '2000-01-01')::text || ',' || time_hash(t::time)::text || ',' || timetz_hash((t::time || '+05:30')::timetz)::text || ',' || hash_array(ARRAY[t::date])::text || ',' || hash_array(ARRAY[t::timestamptz])::text, '|' ORDER BY t)) FROM generate_series('1999-12-31'::timestamp, '2030-01-01', '7 days 3 hours 17 minutes 0.123 seconds') t`},
			{Name: "interval", What: "hashes of equal intervals written differently", SQL: `SELECT interval_hash('1 mon'), interval_hash('30 days'), interval_hash('720 hours'), interval_hash('infinity'), interval_hash('-infinity'), interval_hash('1 day -24 hours')`},
			{Name: "network", What: "hashes of network addresses", SQL: `SELECT md5(string_agg(hashinet(('10.' || g % 256 || '.' || g / 256 % 256 || '.' || g % 7 || '/' || (g % 33))::inet)::text || ',' || hashinet(('2001:db8::' || to_hex(g))::inet)::text || ',' || hashmacaddr(('08:00:2b:01:02:' || lpad(to_hex(g % 256), 2, '0'))::macaddr)::text || ',' || hashmacaddr8(('08:00:2b:01:02:03:04:' || lpad(to_hex(g % 256), 2, '0'))::macaddr8)::text, '|' ORDER BY g)) FROM generate_series(1, 3000) g`},
			{Name: "jsonb", What: "hashes of JSON", SQL: `SELECT md5(string_agg(jsonb_hash(j)::text || ',' || jsonb_hash_extended(j, 8816678312871386365)::text, '|' ORDER BY g)) FROM (SELECT g, jsonb_build_object('k', g, 'f', g / 7.0, 's', md5(g::text), 'a', jsonb_build_array(g % 3, null, true, 'x'), 'n', jsonb_build_object('z', g % 5)) AS j FROM generate_series(1, 3000) g UNION ALL SELECT -1, '{"a":1.0}' UNION ALL SELECT -2, '{"a":1}' UNION ALL SELECT -3, '[]' UNION ALL SELECT -4, 'null' UNION ALL SELECT -5, '"ü"') s`},
			{Name: "range", What: "hashes of ranges", SQL: `SELECT md5(string_agg(hash_range(int4range(g, g + g % 17))::text || ',' || hash_range(numrange(g / 7.0, g / 3.0, '(]'))::text || ',' || hash_range_extended(tstzrange('2020-01-01'::timestamptz + g * interval '1 hour', NULL), 8816678312871386365)::text || ',' || hash_multirange(int4multirange(int4range(g, g + 3), int4range(g + 10, g + 20)))::text, '|' ORDER BY g)) FROM generate_series(1, 3000) g`},
			{Name: "array_record", What: "hashes of arrays and rows", SQL: `SELECT md5(string_agg(hash_array(ARRAY[g, g * 2, NULL])::text || ',' || hash_array(ARRAY['a' || g, NULL, 'ü'])::text || ',' || hash_array_extended(ARRAY[g::numeric / 3], 8816678312871386365)::text || ',' || hash_record(ROW(g, 'x' || g, g::float8 / 3))::text || ',' || hash_array(ARRAY[decode(md5(g::text), 'hex')])::text || ',' || hash_array(ARRAY[(g % 2 = 0)])::text, '|' ORDER BY g)) FROM generate_series(1, 3000) g`},
			{Name: "partition_routing", What: "choosing hash partitions", SQL: `SELECT md5(string_agg(m || ':' || ((((h::numeric + 18446744073709551616) % 18446744073709551616) + 5305509591434766563) % 18446744073709551616 % m)::text, ',' ORDER BY k, m)) FROM (
				SELECT 'i' || g AS k, hashint4extended(g, 8816678312871386365) AS h FROM generate_series(1, 2000) g
				UNION ALL SELECT 'b' || g, hashint8extended(g::int8 * 1000003, 8816678312871386365) FROM generate_series(1, 2000) g
				UNION ALL SELECT 't' || g, hashtextextended(md5(g::text) || 'ü', 8816678312871386365) FROM generate_series(1, 2000) g
				UNION ALL SELECT 'u' || g, uuid_hash_extended(md5(g::text)::uuid, 8816678312871386365) FROM generate_series(1, 2000) g
				UNION ALL SELECT 'n' || g, hash_numeric_extended(g / 7.0, 8816678312871386365) FROM generate_series(1, 2000) g
				UNION ALL SELECT 'd' || g, timestamp_hash_extended('2001-01-01'::timestamp + g * interval '1 day 1 minute', 8816678312871386365) FROM generate_series(1, 2000) g
			) s, (VALUES (2), (3), (4), (7), (16), (100)) AS mm(m)`},
		},
	},
	{
		Name: "time", What: "converting dates and times", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "zones_americas", What: "time zones of the Americas", SQL: tzStatement(probeZones[1:9])},
			{Name: "zones_europe_africa", What: "time zones of Europe and Africa", SQL: tzStatement(probeZones[9:17])},
			{Name: "zones_asia", What: "time zones of Asia", SQL: tzStatement(probeZones[17:24])},
			{Name: "zones_pacific_other", What: "time zones of the Pacific and others", SQL: tzStatement(append([]string{probeZones[0]}, probeZones[24:]...))},
			{Name: "local_gaps", What: "local times that happen twice or not at all", SQL: `SELECT string_agg(z || ' ' || l || ' => ' || (l::timestamp AT TIME ZONE z)::text, '; ' ORDER BY z, l) FROM (VALUES ('America/New_York'), ('Europe/Berlin'), ('Australia/Lord_Howe'), ('Pacific/Apia'), ('America/Santiago'), ('Asia/Gaza')) AS z(z), (VALUES ('2024-03-10 02:30'), ('2024-11-03 01:30'), ('2024-03-31 02:30'), ('2024-10-27 02:30'), ('2024-04-07 01:45'), ('2024-10-06 02:15'), ('2011-12-30 12:00'), ('2024-09-08 00:00')) AS l(l)`},
			{Name: "abbreviations", What: "reading time-zone abbreviations", SQL: `SELECT string_agg(v || ' => ' || v::timestamptz::text, '; ' ORDER BY v) FROM (VALUES ('2024-01-15 12:00 EST'), ('2024-07-15 12:00 EDT'), ('2024-07-15 12:00 CEST'), ('2024-01-15 12:00 IST'), ('2024-01-15 12:00 AEDT'), ('2024-01-15 12:00 NZDT'), ('2024-01-15 12:00 MSK'), ('2024-01-15 12:00 Z'), ('2024-01-15 12:00 +05:45'), ('2024-01-15 12:00 -03:30'), ('2024-01-15 12:00 America/Caracas'), ('2024-01-15T12:00:00.123456789Z'), ('J2451187'), ('January 8, 99 BC'), ('1999-01-08 04:05:06.789 PST8PDT'), ('epoch')) t(v)`},
			{Name: "posix_zones", What: "POSIX-style time zones", SQL: `SELECT string_agg(z || ' ' || ('2024-06-30 12:00+00'::timestamptz AT TIME ZONE z)::text || ' ' || ('2024-12-30 12:00+00'::timestamptz AT TIME ZONE z)::text, '; ' ORDER BY z) FROM (VALUES ('EST5EDT'), ('PST8PDT'), ('UTC+3'), ('<+0330>-3:30'), ('CET-1CEST,M3.5.0,M10.5.0/3'), ('XXX3YYY,M3.2.0,M11.1.0'), ('+05:30'), ('-14')) t(z)`},
			{Name: "far_dates", What: "very old and far-future dates", SQL: `SELECT string_agg(v || ' => ' || v::timestamptz::text || ' / ' || v::date::text || ' / ' || extract(epoch FROM v::timestamptz)::text || ' / ' || extract(julian FROM v::date)::text, '; ' ORDER BY v) FROM (VALUES ('4713-11-24 BC'), ('0001-01-01 BC'), ('0001-01-01'), ('1582-10-04'), ('1582-10-15'), ('1752-09-14'), ('1900-02-28'), ('2000-02-29'), ('2100-02-28'), ('9999-12-31'), ('294276-12-31'), ('5874897-12-31')) t(v) WHERE v NOT IN ('5874897-12-31')`},
			{Name: "far_date_limit", What: "the largest date", SQL: `SELECT '5874897-12-31'::date::text, ('5874897-12-31'::date + 1)::text`},
			{Name: "extract", What: "taking parts of dates and times", SQL: `SELECT md5(string_agg(extract(isoyear FROM t)::text || extract(week FROM t)::text || extract(dow FROM t)::text || extract(isodow FROM t)::text || extract(doy FROM t)::text || extract(quarter FROM t)::text || extract(epoch FROM t)::text || extract(julian FROM t)::text || extract(millennium FROM t)::text || date_trunc('week', t)::text || date_trunc('quarter', t)::text || date_trunc('day', t::timestamptz, 'Asia/Kolkata')::text || date_bin('15 minutes', t, '2001-01-01')::text, '|' ORDER BY t)) FROM generate_series('1899-12-25'::timestamp, '2101-01-07', '3 days 7 hours 11 minutes') t`},
			{Name: "arithmetic", What: "adding and subtracting dates and times", SQL: `SELECT md5(string_agg((t + interval '1 month')::text || (t - interval '1 year 2 months 3 days 04:05:06.789')::text || (t::date + 30)::text || (t - '2000-01-31'::timestamp)::text || age(t, '1999-02-28 23:59:59')::text || justify_interval(t - '1970-01-01')::text || (t::timestamptz + interval '1 day')::text || ((t::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'Europe/Berlin')::text, '|' ORDER BY t)) FROM generate_series('1999-01-31'::timestamp, '2031-01-31', '27 days 13 hours') t`},
			{Name: "dst_series", What: "stepping through days across clock changes", SQL: `SELECT string_agg(t::text, ' ' ORDER BY t) FROM generate_series('2024-03-08 12:00+00'::timestamptz, '2024-03-13 12:00+00', '1 day', 'America/New_York') t`},
			{Name: "make_and_epoch", What: "building times from parts and from seconds", SQL: `SELECT make_timestamptz(2024, 3, 10, 2, 30, 0, 'America/New_York')::text, make_timestamptz(2024, 10, 27, 2, 30, 15.5, 'Europe/Berlin')::text, make_date(-44, 3, 15)::text, make_interval(1, 2, 3, 4, 5, 6, 7.5)::text, to_timestamp(1e9)::text, to_timestamp(1234567890.123456)::text, to_timestamp(-1e10)::text, to_timestamp('infinity')::text`},
			{Name: "interval_io", What: "printing intervals", SQL: `SELECT string_agg(v || ' => ' || v::interval::text, '; ' ORDER BY v) FROM (VALUES ('1 year 2 months 3 days 04:05:06.789'), ('-1 day +02:03'), ('P1Y2M3DT4H5M6.5S'), ('1-2'), ('3 4:05:06'), ('@ 1 hour ago'), ('1.5 months'), ('1.25 years'), ('0.5 days'), ('1 week'), ('infinity'), ('-infinity'), ('178000000 years')) t(v)`},
			{Name: "interval_styles", What: "printing intervals in every style", SQL: `SELECT string_agg(s || ': ' || (SELECT string_agg(v::interval::text, ', ' ORDER BY v) FROM (VALUES ('1 year 2 months 3 days 04:05:06.789'), ('-1 day +02:03'), ('-1 year -2 mons +3 days -04:05:06'), ('0')) t(v)), ' | ' ORDER BY s) FROM (VALUES ('postgres')) x(s)`},
			{Name: "interval_iso", What: "printing intervals in ISO style", Setup: []string{"SET LOCAL IntervalStyle = 'iso_8601'"}, SQL: `SELECT string_agg(v::interval::text, ', ' ORDER BY v) FROM (VALUES ('1 year 2 months 3 days 04:05:06.789'), ('-1 day +02:03'), ('-1 year -2 mons +3 days -04:05:06'), ('0')) t(v)`},
			{Name: "interval_sql", What: "printing intervals in SQL style", Setup: []string{"SET LOCAL IntervalStyle = 'sql_standard'"}, SQL: `SELECT string_agg(v::interval::text, ', ' ORDER BY v) FROM (VALUES ('1 year 2 months 3 days 04:05:06.789'), ('-1 day +02:03'), ('-1 year -2 mons +3 days -04:05:06'), ('0')) t(v)`},
			{Name: "interval_verbose", What: "printing intervals in verbose style", Setup: []string{"SET LOCAL IntervalStyle = 'postgres_verbose'"}, SQL: `SELECT string_agg(v::interval::text, ', ' ORDER BY v) FROM (VALUES ('1 year 2 months 3 days 04:05:06.789'), ('-1 day +02:03'), ('-1 year -2 mons +3 days -04:05:06'), ('0')) t(v)`},
			{Name: "interval_math", What: "interval arithmetic and comparison", SQL: `SELECT md5(string_agg((i * (g::float8 / 7))::text || (i / (g::float8 / 3))::text || (i * g)::text || (i > interval '30 days')::text || (i = interval '1 mon')::text || justify_hours(i * g)::text || extract(epoch FROM i * g)::text, '|' ORDER BY g, i)) FROM generate_series(1, 300) g, (VALUES (interval '1 mon'), (interval '30 days'), (interval '1 year 1 day 00:00:00.000001'), (interval '-5 hours 30 minutes')) AS x(i)`},
			{Name: "datestyles", What: "printing dates in every style", SQL: `SELECT string_agg(t::text, ', ' ORDER BY t) FROM (VALUES ('1999-01-08 04:05:06.789+00'::timestamptz), ('0044-03-15 12:00+00 BC'), ('2024-02-29 23:59:59.999999+00')) x(t)`},
			{Name: "datestyle_sql", What: "printing dates in SQL style", Setup: []string{"SET LOCAL DateStyle = 'SQL, DMY'"}, SQL: `SELECT string_agg(t::text || ' ' || t::date::text || ' ' || ('08/01/1999'::date)::text, ', ' ORDER BY t) FROM (VALUES ('1999-01-08 04:05:06.789+00'::timestamptz), ('0044-03-15 12:00+00 BC'), ('2024-02-29 23:59:59.999999+00')) x(t)`},
			{Name: "datestyle_german", What: "printing dates in German style", Setup: []string{"SET LOCAL DateStyle = 'German'", "SET LOCAL TimeZone = 'Europe/Berlin'"}, SQL: `SELECT string_agg(t::text || ' ' || t::date::text, ', ' ORDER BY t) FROM (VALUES ('1999-01-08 04:05:06.789+00'::timestamptz), ('0044-03-15 12:00+00 BC'), ('2024-02-29 23:59:59.999999+00')) x(t)`},
			{Name: "datestyle_postgres", What: "printing dates in Postgres style", Setup: []string{"SET LOCAL DateStyle = 'Postgres, MDY'", "SET LOCAL TimeZone = 'America/New_York'"}, SQL: `SELECT string_agg(t::text || ' ' || t::date::text, ', ' ORDER BY t) FROM (VALUES ('1999-01-08 04:05:06.789+00'::timestamptz), ('1883-11-18 12:00+00'), ('2024-02-29 23:59:59.999999+00')) x(t)`},
			{Name: "session_zone", What: "printing times in a session time zone", Setup: []string{"SET LOCAL TimeZone = 'Australia/Lord_Howe'"}, SQL: `SELECT md5(string_agg(t::text || '/' || t::timestamp::text || '/' || t::date::text || '/' || t::timetz::text, '|' ORDER BY t)) FROM generate_series('1980-01-01 00:00+00'::timestamptz, '2040-01-01', '5 days 30 minutes') t`},
			{Name: "time_types", What: "time-of-day types", SQL: `SELECT string_agg(v || ' => ' || v::time::text || ' / ' || v::timetz::text, '; ' ORDER BY v) FROM (VALUES ('04:05:06.789'), ('24:00:00'), ('23:59:59.9999995'), ('04:05 PM'), ('04:05:06 PST'), ('04:05:06.789-8'), ('allballs')) t(v)`},
			{Name: "overlaps_ranges", What: "comparing time ranges", SQL: `SELECT ('2024-01-01'::date, '2024-02-01'::date) OVERLAPS ('2024-01-31'::date, '2024-03-01'::date), tstzrange('2024-03-10 01:00-05', '2024-03-10 04:00-04') @> '2024-03-10 03:30 America/New_York'::timestamptz, ('2024-03-10 03:30'::timestamp AT TIME ZONE 'America/New_York')::text`},
		},
	},
	{
		Name: "format", What: "formatting and reading numbers and dates as text", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "to_char_time", What: "formatting dates and times", SQL: `SELECT md5(string_agg(to_char(t, p), '|' ORDER BY t, p)) FROM generate_series('1999-12-25 13:14:15.123456+00'::timestamptz, '2031-01-07', '41 days 17 hours 3 minutes') t, (VALUES ('YYYY-MM-DD HH24:MI:SS.US TZ OF TZH:TZM'), ('Day, DD Month YYYY HH12:MI AM'), ('FMDay FMMonth FMDD'), ('IYYY-IW-ID J Q WW W DDD'), ('Dy Mon DDth YY CC "text" RM rm'), ('SSSS SSSSS FF1 FF3 FF6 MS'), ('TMDay TMMonth'), ('YYYY BC A.D. b.c.'), ('Y,YYY YYY YY Y')) AS p(p)`},
			{Name: "to_char_time_zone", What: "formatting times in another zone", Setup: []string{"SET LOCAL TimeZone = 'America/St_Johns'"}, SQL: `SELECT md5(string_agg(to_char(t, 'YYYY-MM-DD HH24:MI:SS TZ OF TZH:TZM'), '|' ORDER BY t)) FROM generate_series('1960-01-01 00:00+00'::timestamptz, '2040-01-01', '11 days 5 hours') t`},
			{Name: "to_char_number", What: "formatting numbers", SQL: `SELECT md5(string_agg(to_char(n, p), '|' ORDER BY n, p)) FROM (SELECT (g * 1234.5678)::numeric - 600000 AS n FROM generate_series(1, 1000) g UNION ALL SELECT 0 UNION ALL SELECT -0.5 UNION ALL SELECT 0.5) s, (VALUES ('999,999,990.99'), ('FM999999999.0000'), ('S9999999.99'), ('9999999.99PR'), ('9999999MI'), ('L 999G999D99'), ('99999999th'), ('RN'), ('FMRN'), ('9.99EEEE'), ('999V99'), ('0000000000'), ('SG9999999')) AS p(p) WHERE p.p NOT LIKE '%RN%' OR abs(n) BETWEEN 1 AND 3999`},
			{Name: "to_char_int", What: "formatting whole numbers", SQL: `SELECT md5(string_agg(to_char(g, '999,999,999') || to_char(g::int8 * 1000003, 'FM9999999999999999') || to_char(g % 3999 + 1, 'RN') || to_char(g, 'FM9999th'), '|' ORDER BY g)) FROM generate_series(-3000, 3000) g`},
			{Name: "to_date", What: "reading dates from text", SQL: `SELECT string_agg(coalesce(to_date(v, p)::text, 'null'), ', ' ORDER BY v, p) FROM (VALUES ('05 Dec 2000', 'DD Mon YYYY'), ('2000-12-05', 'YYYY-MM-DD'), ('20001205', 'YYYYMMDD'), ('2000 340', 'YYYY DDD'), ('2001-W01-1', 'IYYY-"W"IW-ID'), ('J2451884', '"J"J'), ('Tuesday 5 December 2000', 'Day DD Month YYYY'), ('0044 BC 03 15', 'YYYY BC MM DD'), ('99-1-8', 'YY-MM-DD'), ('69-1-8', 'YY-MM-DD'), ('70-1-8', 'YY-MM-DD')) t(v, p)`},
			{Name: "to_timestamp", What: "reading times from text", SQL: `SELECT string_agg(coalesce(to_timestamp(v, p)::text, 'null'), ', ' ORDER BY v, p) FROM (VALUES ('2011-12-18 11:38 PST', 'YYYY-MM-DD HH12:MI TZ'), ('2011-12-18 23:38:15.123456 +05:30', 'YYYY-MM-DD HH24:MI:SS.US TZH:TZM'), ('18 Dec 2011 11:38 PM', 'DD Mon YYYY HH:MI AM'), ('2011-12-18 23:38:15', 'YYYY-MM-DD HH24:MI:SS'), ('1997 AD 17 DEC 07:37:16', 'YYYY AD DD MON HH24:MI:SS'), ('15:12:02.020001', 'HH24:MI:SS.FF6'), ('2000 + JUN', 'YYYY  MON')) t(v, p)`},
			{Name: "to_number", What: "reading numbers from text", SQL: `SELECT string_agg(coalesce(to_number(v, p)::text, 'null'), ', ' ORDER BY v, p) FROM (VALUES ('-34,338,492', '99G999G999'), ('-34,338,492.654,878', '99G999G999D999G999'), ('<564646.654564>', '999999.999999PR'), ('0.00001-', '9.999999S'), ('5.01-', 'FM9.999999S'), ('$1,234.56', 'L9,999.99'), ('CMXCIX', 'RN'), ('  42', '9999')) t(v, p)`},
			{Name: "format_fn", What: "format() and quoting", SQL: `SELECT md5(string_agg(format('%s|%I|%L|%10s|%-10s|%1$s|%3$s', s, s, s, s, s) || quote_ident(s) || quote_literal(s) || quote_nullable(s) || to_hex(id * 1000003) || lpad(s, 12, '·') || rpad(s, 12, 'ü'), '|' ORDER BY id)) FROM ` + probeWords},
			{Name: "money", What: "the money type", SQL: `SELECT string_agg(v::money::text || '/' || (v::money * 3.5)::text || '/' || (v::money / 7)::text, ', ' ORDER BY v) FROM (VALUES ('12.345'), ('-1234567.891'), ('0'), ('922337203685477.58')) t(v)`},
		},
	},
	{
		Name: "text", What: "handling text (upper and lower case, patterns, sorting)", MustMatch: true,
		Statements: []ProbeStatement{
			// D48: the built-in provider's Unicode tables (compiled into
			// Postgres) decide case and character classes for C.UTF-8 and
			// pg_unicode_fast; sorting under C.UTF-8 is by code point and
			// does not depend on them. The version must match; the
			// statements below check the behaviour itself.
			{Name: "builtin_unicode_version", What: "Unicode version of the built-in text rules", SQL: `SELECT unicode_version() || '/' || (SELECT string_agg(collname || '=' || coalesce(collversion, ''), ',' ORDER BY collname) FROM pg_collation WHERE collprovider = 'b')`},
			{Name: "case_default", What: "upper and lower case (database default)", SQL: `SELECT string_agg(id || ':' || upper(s) || '/' || lower(s) || '/' || initcap(s), '|' ORDER BY id) FROM ` + probeWords},
			{Name: "case_c_utf8", What: "upper and lower case (C.UTF-8)", SQL: `SELECT string_agg(id || ':' || upper(s COLLATE pg_c_utf8) || '/' || lower(s COLLATE pg_c_utf8) || '/' || initcap(s COLLATE pg_c_utf8) || '/' || casefold(s COLLATE pg_c_utf8), '|' ORDER BY id) FROM ` + probeWords},
			{Name: "case_unicode_fast", What: "upper and lower case (full Unicode mapping)", SQL: `SELECT string_agg(id || ':' || upper(s COLLATE pg_unicode_fast) || '/' || lower(s COLLATE pg_unicode_fast) || '/' || initcap(s COLLATE pg_unicode_fast) || '/' || casefold(s COLLATE pg_unicode_fast), '|' ORDER BY id) FROM ` + probeWords},
			{Name: "case_c", What: "upper and lower case (C)", SQL: `SELECT string_agg(id || ':' || upper(s COLLATE "C") || '/' || lower(s COLLATE "C") || '/' || initcap(s COLLATE "C"), '|' ORDER BY id) FROM ` + probeWords},
			{Name: "case_all_codepoints", What: "upper and lower case of every character", SQL: `SELECT md5(string_agg(upper(c) || lower(c) || initcap(c) || casefold(c), '' ORDER BY cp)), md5(string_agg(upper(c COLLATE pg_unicode_fast) || lower(c COLLATE pg_unicode_fast) || casefold(c COLLATE pg_unicode_fast), '' ORDER BY cp)) FROM (SELECT cp, chr(cp) COLLATE pg_c_utf8 AS c FROM generate_series(1, 196607) cp WHERE cp NOT BETWEEN 55296 AND 57343) s`},
			{Name: "classes_all_codepoints", What: "letter, digit and space classes of every character", SQL: `SELECT md5(string_agg(concat((c ~ '[[:alpha:]]')::int, (c ~ '[[:upper:]]')::int, (c ~ '[[:lower:]]')::int, (c ~ '[[:digit:]]')::int, (c ~ '[[:space:]]')::int, (c ~ '[[:punct:]]')::int, (c ~ '[[:alnum:]]')::int, (c ~ '[[:print:]]')::int, (c ~ '[[:graph:]]')::int, (c ~ '[[:cntrl:]]')::int, (c ~ '[[:xdigit:]]')::int, (c ~ '\w')::int, (c ~* 'a')::int), '' ORDER BY cp)) FROM (SELECT cp, chr(cp) COLLATE pg_c_utf8 AS c FROM generate_series(1, 196607) cp WHERE cp NOT BETWEEN 55296 AND 57343) s`},
			{Name: "classes_c", What: "letter classes under C", SQL: `SELECT md5(string_agg(concat((c ~ '[[:alpha:]]')::int, (c ~ '[[:upper:]]')::int, (c ~ '[[:space:]]')::int, (c ~ '\w')::int, (c ~* 'é')::int, upper(c)), '' ORDER BY cp)) FROM (SELECT cp, chr(cp) COLLATE "C" AS c FROM generate_series(1, 70000) cp WHERE cp NOT BETWEEN 55296 AND 57343) s`},
			{Name: "ilike", What: "case-insensitive matching", SQL: `SELECT string_agg(a.id || '~' || b.id || '=' || (a.s ILIKE b.s)::int || (a.s ~* ('^' || regexp_replace(b.s, '([^[:alnum:] ])', '\\\1', 'g') || '$'))::int || (lower(a.s) = lower(b.s))::int, ',' ORDER BY a.id, b.id) FROM ` + probeWords + ` JOIN ` + strings.Replace(probeWords, "AS w(id, s)", "AS b(id, s)", 1) + ` ON true JOIN ` + strings.Replace(probeWords, "AS w(id, s)", "AS a(id, s)", 1) + ` ON a.id = w.id WHERE a.s <> '' AND b.s <> ''`},
			{Name: "like_patterns", What: "LIKE patterns on many scripts", SQL: `SELECT string_agg(id || ':' || (s LIKE '%a%')::int || (s LIKE '_%')::int || (s ILIKE '%Σ%')::int || (s ILIKE '%ß%')::int || (s ILIKE '%i%')::int || (s ILIKE '%İ%')::int || (s SIMILAR TO '%(é|e)%')::int, ',' ORDER BY id) FROM ` + probeWords},
			{Name: "regex_functions", What: "regular expressions", SQL: `SELECT md5(string_agg(regexp_replace(s, '[[:lower:]]', '*', 'g') || '|' || regexp_replace(s, '\w+', '<\&>', 'g') || '|' || regexp_count(s, '[[:alpha:]]')::text || '|' || coalesce(array_to_string(regexp_match(s, '([[:upper:]]+)(.)'), ','), '-') || '|' || array_to_string(regexp_split_to_array(s, '\s+'), ',') || '|' || regexp_like(s, 'STRASSE|İSTANBUL|αθηνα', 'i')::text || '|' || regexp_instr(s, '[^[:ascii:]]')::text || '|' || coalesce(substring(s from '[[:digit:]]+'), '-'), '#' ORDER BY id)) FROM ` + probeWords},
			{Name: "string_functions", What: "text functions", SQL: `SELECT md5(string_agg(length(s)::text || ',' || octet_length(s)::text || ',' || reverse(s) || ',' || left(s, 3) || ',' || right(s, 2) || ',' || translate(s, 'aeiouß', 'AEIOU?') || ',' || strpos(s, 'a')::text || ',' || split_part(s || ' x', ' ', 2) || ',' || starts_with(s, 'A')::text || ',' || replace(s, 'a', 'ä') || ',' || overlay(s placing 'ü' from 2 for 1) || ',' || coalesce(ascii(s)::text, '') || ',' || md5(s) || ',' || btrim(s) || ',' || encode(convert_to(s, 'UTF8'), 'hex'), '|' ORDER BY id)) FROM ` + probeWords + ` WHERE s ~ '^[ -~À-ÿ]*$'`},
			{Name: "normalize", What: "Unicode normalisation", SQL: `SELECT string_agg(id || ':' || encode(convert_to(normalize(s, NFC), 'UTF8'), 'hex') || '/' || encode(convert_to(normalize(s, NFD), 'UTF8'), 'hex') || '/' || encode(convert_to(normalize(s, NFKC), 'UTF8'), 'hex') || '/' || encode(convert_to(normalize(s, NFKD), 'UTF8'), 'hex') || '/' || (s IS NFC NORMALIZED)::int || (s IS NFKD NORMALIZED)::int || unicode_assigned(s)::int, '|' ORDER BY id) FROM ` + probeWords},
			{Name: "normalize_all_codepoints", What: "normalising every character", SQL: `SELECT md5(string_agg(normalize(c, NFD) || normalize(c, NFKC) || unicode_assigned(c)::int::text, '' ORDER BY cp)) FROM (SELECT cp, chr(cp) AS c FROM generate_series(160, 196607) cp WHERE cp NOT BETWEEN 55296 AND 57343) s`},
			{Name: "sort_default", What: "sorting text (database default)", SQL: `SELECT string_agg(id::text, ',' ORDER BY s, id) FROM ` + probeWords},
			{Name: "sort_c", What: "sorting text (C)", SQL: `SELECT string_agg(id::text, ',' ORDER BY s COLLATE "C", id) FROM ` + probeWords},
			{Name: "sort_c_utf8", What: "sorting text (C.UTF-8)", SQL: `SELECT string_agg(id::text, ',' ORDER BY s COLLATE pg_c_utf8, id), string_agg(id::text, ',' ORDER BY lower(s COLLATE pg_c_utf8), id), string_agg(id::text, ',' ORDER BY upper(s COLLATE pg_c_utf8) DESC, id) FROM ` + probeWords},
			{Name: "sort_unicode_fast", What: "sorting text (full Unicode mapping)", SQL: `SELECT string_agg(id::text, ',' ORDER BY s COLLATE pg_unicode_fast, id), string_agg(id::text, ',' ORDER BY casefold(s COLLATE pg_unicode_fast), id) FROM ` + probeWords},
			{Name: "sort_ucs_basic", What: "sorting text (ucs_basic)", SQL: `SELECT string_agg(id::text, ',' ORDER BY s COLLATE ucs_basic, id) FROM ` + probeWords},
			{Name: "sort_codepoints", What: "sorting characters from every script", SQL: `SELECT md5(string_agg(cp::text, ',' ORDER BY c, cp)), md5(string_agg(cp::text, ',' ORDER BY c COLLATE "C" DESC, cp)) FROM (SELECT cp, chr(cp) || chr((cp * 7) % 50000 + 32) AS c FROM generate_series(32, 196607, 13) cp WHERE cp NOT BETWEEN 55296 AND 57343 AND (cp * 7) % 50000 + 32 NOT BETWEEN 55296 AND 57343) s`},
			{Name: "compare_pairs", What: "comparing pairs of words", SQL: `SELECT md5(string_agg((a.s < b.s)::int::text || (a.s COLLATE "C" < b.s)::int::text || (a.s COLLATE pg_unicode_fast < b.s)::int::text || (a.s = b.s)::int::text || (lower(a.s) = lower(b.s))::int::text, '' ORDER BY a.id, b.id)) FROM ` + strings.Replace(probeWords, "AS w(id, s)", "AS a(id, s)", 1) + `, ` + strings.Replace(probeWords, "AS w(id, s)", "AS b(id, s)", 1)},
			{Name: "encodings", What: "converting text between encodings", SQL: `SELECT string_agg(id || ':' || coalesce(encode(convert_to(s, 'LATIN1'), 'hex'), ''), '|' ORDER BY id) FROM ` + probeWords + ` WHERE s ~ '^[ -~À-ÿ]*$'`},
		},
	},
	{
		Name: "fts", What: "full-text search", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "english", What: "English text search", SQL: `SELECT to_tsvector('english', 'The quick brown foxes were jumping over the lazy dogs; it''s a dog-eat-dog world, isn''t it? Running runners ran. Generously generous generalisation, 3.14 and user@example.com http://example.com/path?q=1')::text, to_tsquery('english', 'foxes & (jumping | running) & !cats')::text, plainto_tsquery('english', 'The Fat Rats')::text, phraseto_tsquery('english', 'lazy dogs jumped')::text, websearch_to_tsquery('english', '"quick fox" -lazy or dog')::text`},
			{Name: "simple", What: "simple text search on many scripts", SQL: `SELECT md5(string_agg(to_tsvector('simple', s)::text || '/' || plainto_tsquery('simple', s)::text, '|' ORDER BY id)) FROM ` + probeWords},
			{Name: "languages", What: "text search in other languages", SQL: `SELECT string_agg(c || ': ' || to_tsvector(c::regconfig, t)::text, ' | ' ORDER BY c) FROM (VALUES
				('german', 'Die Häuser der Straße waren größer als die Bäume. Übermäßige Fußgänger.'),
				('french', 'Les élèves étudiaient tranquillement leurs leçons dans les bibliothèques.'),
				('spanish', 'Los niños corrían rápidamente hacia las montañas nevadas.'),
				('russian', 'Москва — столица России. Студенты читали интересные книги.'),
				('turkish', 'İstanbul''daki ışıklı caddelerde yürüyorduk. Kitaplarımızı okuduk.'),
				('dutch', 'De kinderen speelden vrolijk in de tuinen van hun grootouders.'),
				('finnish', 'Talojen ikkunoista näkyi järvien rannoilla kasvavia puita.'),
				('italian', 'Gli studenti leggevano attentamente i loro libri preferiti.'),
				('portuguese', 'As crianças brincavam alegremente nas praias ensolaradas.'),
				('swedish', 'Barnen lekte glatt i trädgårdarna bakom husen.'),
				('danish', 'Børnene legede glade i haverne bag husene.'),
				('norwegian', 'Barna lekte glade i hagene bak husene.'),
				('hungarian', 'A gyerekek vidáman játszottak a kertekben.'),
				('romanian', 'Copiii se jucau veseli în grădinile caselor.'),
				('greek', 'Τα παιδιά έπαιζαν χαρούμενα στους κήπους.'),
				('arabic', 'كان الأطفال يلعبون بسعادة في الحدائق'),
				('indonesian', 'Anak-anak bermain dengan gembira di taman.'),
				('lithuanian', 'Vaikai linksmai žaidė soduose.'),
				('nepali', 'केटाकेटीहरू बगैंचामा खुसीसाथ खेलिरहेका थिए।'),
				('tamil', 'குழந்தைகள் தோட்டங்களில் மகிழ்ச்சியாக விளையாடினர்.'),
				('armenian', 'Երեխաները ուրախ խաղում էին այգիներում։'),
				('basque', 'Haurrak pozik jolasten ziren lorategietan.'),
				('catalan', 'Els nens jugaven feliços als jardins.'),
				('estonian', 'Lapsed mängisid rõõmsalt aedades.'),
				('irish', 'Bhí na páistí ag súgradh go sona sna gairdíní.'),
				('serbian', 'Деца су се весело играла у баштама.'),
				('yiddish', 'די קינדער האָבן זיך פֿריילעך געשפּילט אין די גערטנער.'),
				('english', 'Children were playing happily in the gardens.')) t(c, t) WHERE EXISTS (SELECT 1 FROM pg_ts_config WHERE cfgname = c)`},
			{Name: "parser", What: "splitting text into words", SQL: `SELECT string_agg(tokid || ':' || token, ' ' ORDER BY ord) FROM ts_parse('default', 'foo-bar 3.14 -2 1e10 http://x.org/a?b=c user@host.com <b>tag</b> &amp; Ünïcode αβγ 北京 2024-01-01 +7 /usr/local/bin v1.2.3') WITH ORDINALITY AS p(tokid, token, ord)`},
			{Name: "lexize", What: "dictionary lookups", SQL: `SELECT string_agg(d || ':' || coalesce(array_to_string(ts_lexize(d::regdictionary, w), ','), 'null'), ' ' ORDER BY d, w) FROM (VALUES ('english_stem'), ('simple'), ('german_stem'), ('russian_stem'), ('french_stem'), ('spanish_stem'), ('english_stem')) d(d), (VALUES ('running'), ('the'), ('Häuser'), ('столица'), ('généralement'), ('corriendo'), ('a')) w(w)`},
			{Name: "operations", What: "text-search operations", SQL: `SELECT md5(string_agg(setweight(v, 'A')::text || strip(v)::text || (v || to_tsvector('simple', 'extra word'))::text || ts_delete(v, 'quick')::text || array_to_string(tsvector_to_array(v), ',') || (v @@ to_tsquery('english', 'fox <-> jump'))::text || (v @@ to_tsquery('english', 'quick & !slow'))::text || ts_headline('english', d, to_tsquery('english', 'fox'))::text, '|' ORDER BY g)) FROM (SELECT g, d, to_tsvector('english', d) AS v FROM (SELECT g, 'The quick brown fox jumps over ' || g || ' lazy dogs and ' || md5(g::text) AS d FROM generate_series(1, 500) g) s) t`},
			{Name: "stopwords", What: "stop-word lists", SQL: `SELECT string_agg(c || ':' || to_tsvector(c::regconfig, 'the a and of der die das le la les el los и в не ve bir och og en het de at')::text, ' | ' ORDER BY c) FROM (VALUES ('english'), ('german'), ('french'), ('spanish'), ('russian'), ('turkish'), ('swedish'), ('danish'), ('norwegian'), ('dutch'), ('finnish'), ('italian'), ('portuguese'), ('hungarian')) t(c)`},
			{Name: "query_rewrite", What: "text-search query handling", SQL: `SELECT numnode(q)::text || ' ' || querytree(q) || ' ' || ts_rewrite(q, 'fox'::tsquery, 'wolf | coyote'::tsquery)::text || ' ' || tsquery_phrase(q, 'dog'::tsquery, 2)::text FROM (SELECT to_tsquery('english', '(quick | fast) & fox & !slow') AS q) s`},
		},
	},
	{
		Name: "jsonb", What: "handling JSON", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "order", What: "sorting JSON values", SQL: `SELECT string_agg(j::text, ' ' ORDER BY j, j::text) FROM (VALUES ('null'::jsonb), ('true'), ('false'), ('0'), ('-1.5'), ('1e2'), ('100'), ('"a"'), ('"B"'), ('"ä"'), ('""'), ('[]'), ('[1]'), ('[1,2]'), ('[2]'), ('{}'), ('{"a":1}'), ('{"b":0}'), ('{"a":1,"b":2}'), ('{"aa":1}'), ('[null]'), ('"é"'), ('"é"')) t(j)`},
			{Name: "normalise", What: "storing JSON (keys, numbers, duplicates)", SQL: `SELECT string_agg(v || ' => ' || v::jsonb::text || ' / ' || v::json::text, ' | ' ORDER BY v) FROM (VALUES ('{"b":1,"a":2,"b":3}'), ('{"a":1.0,"c":1.00e1,"b":-0}'), ('[1e400, 0.1e-400]'), ('{"ü":"ü","€":"€","x":"😀"}'), ('{"a" : [ 1 , { "b" : null } ] }'), ('12345678901234567890123456789.123456789')) t(v)`},
			{Name: "containment", What: "JSON containment and keys", SQL: `SELECT md5(string_agg((a @> b)::int::text || (a <@ b)::int::text || (a ? 'k')::int::text || (a ?| ARRAY['a', 'x'])::int::text || (a ?& ARRAY['k', 's'])::int::text || (a = b)::int::text || (a < b)::int::text, '' ORDER BY ga, gb)) FROM (SELECT g AS ga, jsonb_build_object('k', g % 5, 's', (g % 3)::text, 'a', jsonb_build_array(g % 2, 'x')) AS a FROM generate_series(1, 60) g) x, (SELECT g AS gb, CASE g % 4 WHEN 0 THEN jsonb_build_object('k', g % 5) WHEN 1 THEN jsonb_build_object('a', jsonb_build_array(g % 2)) WHEN 2 THEN jsonb_build_object('s', (g % 3)::text) ELSE '{}'::jsonb END AS b FROM generate_series(1, 60) g) y`},
			{Name: "functions", What: "JSON functions", SQL: `SELECT md5(string_agg(jsonb_pretty(j) || jsonb_strip_nulls(j)::text || jsonb_set(j, '{n,z}', '"new"')::text || (j - 's')::text || (j #> '{a,1}')::text || jsonb_typeof(j -> 'f') || coalesce(jsonb_path_query_first(j, '$.a[*] ? (@ == "x")')::text, '') || jsonb_path_exists(j, '$.s ? (@ like_regex "^[a-f0-9]+$" flag "i")')::text || (SELECT string_agg(k, ',' ORDER BY k) FROM jsonb_object_keys(j) k), '|' ORDER BY g)) FROM (SELECT g, jsonb_build_object('k', g, 'f', g / 7.0, 's', md5(g::text), 'a', jsonb_build_array(g % 3, null, true, 'x'), 'n', jsonb_build_object('z', g % 5)) AS j FROM generate_series(1, 1000) g) s`},
			{Name: "path_datetime", What: "dates inside JSON", SQL: `SELECT string_agg(coalesce(jsonb_path_query_first(to_jsonb(v), '$.datetime()')::text, 'null') || ' ' || coalesce(jsonb_path_query_first_tz(to_jsonb(v), '$.timestamp_tz()', '{}', true)::text, 'null'), ' | ' ORDER BY v) FROM (VALUES ('2024-03-10'), ('2024-03-10T02:30:00'), ('2024-03-10T02:30:00+05:30'), ('12:34:56'), ('12:34:56-08')) t(v)`},
			{Name: "numbers", What: "numbers inside JSON", SQL: `SELECT string_agg((v::jsonb)::text || '/' || (v::jsonb)::numeric::text || '/' || jsonb_path_query_first(v::jsonb, '$.double()')::text || '/' || jsonb_path_query_first(v::jsonb, '$.ceiling()')::text, ' ' ORDER BY v) FROM (VALUES ('0.1'), ('1e-5'), ('123456789.987654321'), ('-0'), ('1E+2'), ('2.5')) t(v)`},
		},
	},
	{
		Name: "types", What: "handling other data types", MustMatch: true,
		Statements: []ProbeStatement{
			{Name: "uuid", What: "uuids", SQL: `SELECT md5(string_agg(u::text || (u < '80000000-0000-0000-0000-000000000000'::uuid)::int::text || coalesce(uuid_extract_version(u)::text, '-'), ',' ORDER BY u)), string_agg(v::uuid::text, ' ' ORDER BY v) FROM (SELECT md5(g::text)::uuid AS u FROM generate_series(1, 3000) g) s, (VALUES ('A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11'), ('{a0eebc99-9c0b4ef8-bb6d6bb9-bd380a11}'), ('a0eebc999c0b4ef8bb6d6bb9bd380a11'), ('017f22e2-79b0-7cc3-98c4-dc0c0c07398f')) t(v) GROUP BY ()`},
			{Name: "uuid_time", What: "time inside uuids", SQL: `SELECT uuid_extract_timestamp('017f22e2-79b0-7cc3-98c4-dc0c0c07398f')::text, uuid_extract_timestamp('c232ab00-9414-11ec-b3c8-9f6bdeced846')::text, uuid_extract_version('c232ab00-9414-11ec-b3c8-9f6bdeced846')`},
			{Name: "inet", What: "network addresses", SQL: `SELECT md5(string_agg(i::text || ',' || host(i) || ',' || masklen(i)::text || ',' || family(i)::text || ',' || network(i)::text || ',' || broadcast(i)::text || ',' || abbrev(i) || ',' || set_masklen(i, 16)::text || ',' || (i << '10.0.0.0/8'::inet)::int::text || (i && '2001:db8::/32'::inet)::int::text || ',' || CASE WHEN family(i) = 4 THEN inet_merge(i, '10.1.2.3')::text ELSE '' END , '|' ORDER BY i, g)) FROM (SELECT g, ('10.' || g % 256 || '.' || g / 256 % 256 || '.' || g % 7 || '/' || (g % 25 + 8))::inet AS i FROM generate_series(1, 2000) g UNION ALL SELECT g, ('2001:db8:' || to_hex(g) || '::' || to_hex(g * 7) || '/' || (g % 128 + 1))::inet FROM generate_series(1, 1000) g) s WHERE family(i) = 4 OR true`},
			{Name: "cidr_macaddr", What: "network types", SQL: `SELECT string_agg(v::cidr::text || '/' || v::cidr::inet::text, ' ' ORDER BY v::cidr), (SELECT string_agg(m::macaddr::text || '/' || trunc(m::macaddr)::text || '/' || m::macaddr8::text, ' ' ORDER BY m::macaddr) FROM (VALUES ('08:00:2b:01:02:03'), ('08-00-2b-01-02-03'), ('08002b:010203'), ('0800.2b01.0203'), ('ff:ff:ff:ff:ff:ff')) x(m)) FROM (VALUES ('192.168.100.128/25'), ('192.168/24'), ('10'), ('2001:4f8:3:ba::/64'), ('::ffff:1.2.3.0/120'), ('0.0.0.0/0')) t(v)`},
			{Name: "ranges", What: "ranges", SQL: `SELECT md5(string_agg(r::text || (r && int4range(100, 200))::int::text || (r @> 150)::int::text || (r -|- int4range(50, 60))::int::text || coalesce((r * int4range(100, 300))::text, '') || lower(r)::text || upper_inf(r)::int::text || range_merge(r, int4range(0, 1))::text, '|' ORDER BY r, g)) FROM (SELECT g, int4range(g * 7 % 500, g * 7 % 500 + g % 23, CASE g % 4 WHEN 0 THEN '[]' WHEN 1 THEN '(]' WHEN 2 THEN '()' ELSE '[)' END) AS r FROM generate_series(1, 2000) g) s`},
			{Name: "range_types", What: "ranges of other types", SQL: `SELECT string_agg(v, ' ' ORDER BY v) FROM (SELECT numrange(1.5, 2.25, '(]')::text UNION ALL SELECT daterange('2024-01-01', '2024-01-31', '[]')::text UNION ALL SELECT tsrange('2024-03-10 02:00', '2024-03-10 03:00')::text UNION ALL SELECT tstzrange('2024-03-10 01:00-05', '2024-03-10 04:00-04')::text UNION ALL SELECT int8range(NULL, 10)::text UNION ALL SELECT int4range(5, 5)::text UNION ALL SELECT '[1,3]'::int4range::text UNION ALL SELECT '(,)'::daterange::text) t(v)`},
			{Name: "multiranges", What: "multiranges", SQL: `SELECT md5(string_agg(m::text || (m @> (g % 50))::int::text || range_merge(m)::text || (m + int4multirange(int4range(g, g + 5)))::text || (m - int4multirange(int4range(10, 20)))::text, '|' ORDER BY m, g)) FROM (SELECT g, int4multirange(int4range(g % 30, g % 30 + 5), int4range(g % 40 + 20, g % 40 + 22), int4range(g % 7, g % 7 + 1)) AS m FROM generate_series(1, 2000) g) s`},
			{Name: "arrays", What: "arrays", SQL: `SELECT md5(string_agg(a::text || (a < ARRAY[5, 5])::int::text || (a @> ARRAY[3])::int::text || (a && ARRAY[1, 9])::int::text || array_to_string(a, ',', '*') || coalesce(array_position(a, 3)::text, '') || cardinality(a)::text || array_dims(a), '|' ORDER BY a, g)) FROM (SELECT g, CASE WHEN g % 11 = 0 THEN ARRAY[NULL, g % 7]::int[] ELSE ARRAY(SELECT (g * x) % 10 FROM generate_series(1, g % 5) x) END AS a FROM generate_series(1, 2000) g) s`},
			{Name: "array_text", What: "text arrays", SQL: `SELECT string_agg(a::text, ' ' ORDER BY a), string_agg(v::text[]::text, ' ' ORDER BY v) FROM (SELECT ARRAY[s, upper(s), NULL] AS a FROM ` + probeWords + `) s, (VALUES ('{a,"b c",NULL,"NULL","\"q\""}'), ('[0:1]={x,y}'), ('{{1,2},{3,4}}'), ('{}')) t(v) GROUP BY ()`},
			{Name: "bytea", What: "binary data", SQL: `SELECT md5(string_agg(b::text || (b < '\x80'::bytea)::int::text || encode(b, 'base64') || encode(b, 'escape') || encode(b, 'hex') || length(b)::text || coalesce(get_byte(b, 0)::text, '') || bit_count(b)::text || md5(b) || encode(sha256(b), 'hex') || encode(sha512(b), 'hex') || encode(sha224(b), 'hex') || encode(sha384(b), 'hex') || crc32(b)::text || crc32c(b)::text || encode(reverse(b), 'hex'), '|' ORDER BY b, g)) FROM (SELECT g, decode(repeat(md5(g::text), g % 4), 'hex') || int4send(g) AS b FROM generate_series(0, 2000) g) s`},
			{Name: "decode", What: "decoding text into binary", SQL: `SELECT string_agg(encode(decode(v, f), 'hex'), ' ' ORDER BY v, f) FROM (VALUES ('aGVsbG8gd29ybGQ=', 'base64'), ('aGVsbG8 gd29y bGQ=', 'base64'), ('68656c6c6f', 'hex'), ('a\\000b\101', 'escape')) t(v, f)`},
			{Name: "bit", What: "bit strings", SQL: `SELECT md5(string_agg(b::text || (b & B'10101010')::text || (b # B'11110000')::text || (b << 3)::text || bit_count(b)::text || get_bit(b, 3)::text || b::int4::text || (b < B'10000000')::int::text, '|' ORDER BY b, g)) FROM (SELECT g, (g % 256)::bit(8) AS b FROM generate_series(1, 1000) g) s`},
			{Name: "boolean_misc", What: "booleans, enums of text, and other small types", SQL: `SELECT string_agg(v::bool::text, ',' ORDER BY v), (SELECT string_agg(x::pg_lsn::text || '/' || (x::pg_lsn - '0/0'::pg_lsn)::text, ',' ORDER BY x) FROM (VALUES ('0/0'), ('16/B374D848'), ('FFFFFFFF/FFFFFFFF')) l(x)), (SELECT string_agg(t::tid::text, ',' ORDER BY t::tid) FROM (VALUES ('(0,1)'), ('(4294967295,65535)'), ('(10,3)')) d(t)), ('abc'::"char")::text, 'x'::char(5) = 'x    '::char(5), length('ab  '::char(4)), octet_length('ab  '::varchar(4)) FROM (VALUES ('t'), ('yes'), ('on'), ('1'), ('f'), ('NO'), ('off'), ('0')) t(v)`},
			{Name: "xid_types", What: "transaction-id types", SQL: `SELECT '42'::xid::text, '4294967295'::xid::text, '42'::xid8::text, ('9223372036854775807'::xid8)::text, ('0'::xid8 < '1'::xid8)::text`},
		},
	},
}
