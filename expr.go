package main

import (
	"container/heap"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JaderDias/movingmedian"
	"github.com/datastream/holtwinters"
	pb "github.com/dgryski/carbonzipper/carbonzipperpb"
	"github.com/dgryski/go-onlinestats"
	"github.com/gogo/protobuf/proto"
	"github.com/wangjohn/quickselect"
)

// expression parser

type exprType int

const (
	etName exprType = iota
	etFunc
	etConst
	etString
	etKV
)

type expr struct {
	target    string
	etype     exprType
	key       string
	val       float64
	valStr    string
	args      []*expr
	argString string
}

type metricRequest struct {
	metric string
	from   int32
	until  int32
}

func (e *expr) metrics() []metricRequest {

	switch e.etype {
	case etName:
		return []metricRequest{{metric: e.target}}
	case etConst, etString:
		return nil
	case etFunc:
		var r []metricRequest
		for _, a := range e.args {
			r = append(r, a.metrics()...)
		}

		switch e.target {
		case "timeShift":
			offs, err := getIntervalArg(e, 1, -1)
			if err != nil {
				return nil
			}
			for i := range r {
				r[i].from += offs
				r[i].until += offs
			}
		case "timeStack":
			offs, err := getIntervalArg(e, 1, -1)
			if err != nil {
				return nil
			}

			start, err := getIntArg(e, 2)
			if err != nil {
				return nil
			}

			end, err := getIntArg(e, 3)
			if err != nil {
				return nil
			}

			var r2 []metricRequest
			for _, v := range r {
				for i := int32(start); i < int32(end); i++ {
					r2 = append(r2, metricRequest{
						metric: v.metric,
						from:   v.from + (i * offs),
						until:  v.until + (i * offs),
					})
				}
			}

			return r2
		case "holtWintersForecast":
			for i := range r {
				r[i].from -= 7 * 86400 // starts -7 days from where the original starts
			}
		}
		return r
	}

	return nil
}

func parseExpr(e string) (*expr, string, error) {

	// skip whitespace
	for len(e) > 1 && e[0] == ' ' {
		e = e[1:]
	}

	if len(e) == 0 {
		return nil, "", ErrMissingExpr
	}

	if '0' <= e[0] && e[0] <= '9' || e[0] == '-' || e[0] == '+' {
		val, e, err := parseConst(e)
		return &expr{val: val, etype: etConst}, e, err
	}

	if e[0] == '\'' || e[0] == '"' {
		val, e, err := parseString(e)
		return &expr{valStr: val, etype: etString}, e, err
	}

	name, e := parseName(e)

	if name == "" {
		return nil, e, ErrMissingArgument
	}

	if e != "" && e[0] == '(' {
		exp := &expr{target: name, etype: etFunc}

		argString, args, e, err := parseArgList(e)
		exp.argString = argString
		exp.args = args

		return exp, e, err
	}

	return &expr{target: name}, e, nil
}

var (
	ErrMissingExpr         = errors.New("missing expression")
	ErrMissingComma        = errors.New("missing comma")
	ErrMissingQuote        = errors.New("missing quote")
	ErrUnexpectedCharacter = errors.New("unexpected character")
)

func parseArgList(e string) (string, []*expr, string, error) {

	var args []*expr

	if e[0] != '(' {
		panic("arg list should start with paren")
	}

	argString := e[1:]

	e = e[1:]

	for {
		var arg *expr
		var err error
		arg, e, err = parseExpr(e)
		if err != nil {
			return "", nil, e, err
		}

		// we now know we're parsing a key-value pair
		if arg.etype == etName && e[0] == '=' {
			e = e[1:]
			argCont, eCont, errCont := parseExpr(e)
			if errCont != nil {
				return "", nil, eCont, errCont
			}

			if argCont.etype != etConst && argCont.etype != etName && argCont.etype != etString {
				return "", nil, eCont, ErrBadType
			}

			arg = &expr{
				etype:  etKV,
				key:    arg.target,
				val:    argCont.val,
				valStr: argCont.valStr,
				target: argCont.target,
			}

			e = eCont
		}

		args = append(args, arg)

		if e == "" {
			return "", nil, "", ErrMissingComma
		}

		if e[0] == ')' {
			return argString[:len(argString)-len(e)], args, e[1:], nil
		}

		if e[0] != ',' && e[0] != ' ' {
			return "", nil, "", ErrUnexpectedCharacter
		}

		e = e[1:]
	}
}

func isNameChar(r byte) bool {
	return false ||
		'a' <= r && r <= 'z' ||
		'A' <= r && r <= 'Z' ||
		'0' <= r && r <= '9' ||
		r == '.' || r == '_' || r == '-' || r == '*' || r == '?' || r == ':' ||
		r == '[' || r == ']'
}

func isDigit(r byte) bool {
	return '0' <= r && r <= '9'
}

func parseConst(s string) (float64, string, error) {

	var i int
	// All valid characters for a floating-point constant
	// Just slurp them all in and let ParseFloat sort 'em out
	for i < len(s) && (isDigit(s[i]) || s[i] == '.' || s[i] == '+' || s[i] == '-' || s[i] == 'e' || s[i] == 'E') {
		i++
	}

	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, "", err
	}

	return v, s[i:], err
}

func parseName(s string) (string, string) {

	var i int

FOR:
	for braces := 0; i < len(s); i++ {

		if isNameChar(s[i]) {
			continue
		}

		switch s[i] {
		case '{':
			braces++
		case '}':
			if braces == 0 {
				break FOR

			}
			braces--
		case ',':
			if braces == 0 {
				break FOR
			}
		default:
			break FOR
		}

	}

	if i == len(s) {
		return s, ""
	}

	return s[:i], s[i:]
}

func parseString(s string) (string, string, error) {

	if s[0] != '\'' && s[0] != '"' {
		panic("string should start with open quote")
	}

	match := s[0]

	s = s[1:]

	var i int
	for i < len(s) && s[i] != match {
		i++
	}

	if i == len(s) {
		return "", "", ErrMissingQuote

	}

	return s[:i], s[i+1:], nil
}

var (
	ErrBadType           = errors.New("bad type")
	ErrMissingArgument   = errors.New("missing argument")
	ErrMissingTimeseries = errors.New("missing time series")
)

func getStringArg(e *expr, n int) (string, error) {
	if len(e.args) <= n {
		return "", ErrMissingArgument
	}

	return doGetStringArgDefault(e.args[n], "")
}

func getStringArgDefault(e *expr, n int, s string) (string, error) {
	if len(e.args) <= n {
		return s, nil
	}

	return doGetStringArgDefault(e.args[n], s)
}

func getStringNamedOrPosArgDefault(e *expr, k string, n int, s string) (string, error) {
	if len(e.args) <= n {
		return s, nil
	}

	if a := getNamedArg(e.args, k); a != nil {
		return doGetStringArgDefault(a, s)
	}

	return getStringArgDefault(e, n, s)
}

func doGetStringArgDefault(e *expr, s string) (string, error) {
	if e.etype != etString && e.etype != etKV {
		return "", ErrBadType
	}

	v := e.valStr
	if v == "" {
		return s, nil
	}

	return v, nil
}

func getIntervalArg(e *expr, n int, defaultSign int) (int32, error) {
	if len(e.args) <= n {
		return 0, ErrMissingArgument
	}

	if e.args[n].etype != etString {
		return 0, ErrBadType
	}

	seconds, err := intervalString(e.args[n].valStr, defaultSign)
	if err != nil {
		return 0, ErrBadType
	}

	return seconds, nil
}

func getFloatArg(e *expr, n int) (float64, error) {
	if len(e.args) <= n {
		return 0, ErrMissingArgument
	}

	return doGetFloatArgDefault(e.args[n], 0)
}

func getFloatArgDefault(e *expr, n int, v float64) (float64, error) {
	if len(e.args) <= n {
		return v, nil
	}

	return doGetFloatArgDefault(e.args[n], v)
}

func getFloatNamedOrPosArgDefault(e *expr, k string, n int, v float64) (float64, error) {
	if len(e.args) <= n {
		return v, nil
	}

	if a := getNamedArg(e.args, k); a != nil {
		return doGetFloatArgDefault(a, v)
	}

	return getFloatArgDefault(e, n, v)
}

func doGetFloatArgDefault(e *expr, v float64) (float64, error) {
	if e.etype != etConst && e.etype != etKV {
		return 0, ErrBadType
	}

	f := e.val
	if f == 0 {
		return v, nil
	}

	return f, nil
}

func getIntArg(e *expr, n int) (int, error) {
	if len(e.args) <= n {
		return 0, ErrMissingArgument
	}

	return doGetIntArgDefault(e.args[n], 0)
}

func getIntArgs(e *expr, n int) ([]int, error) {

	if len(e.args) <= n {
		return nil, ErrMissingArgument
	}

	var ints []int

	for i := n; i < len(e.args); i++ {
		a, err := getIntArg(e, i)
		if err != nil {
			return nil, err
		}
		ints = append(ints, a)
	}

	return ints, nil
}

func getIntArgDefault(e *expr, n int, d int) (int, error) {
	if len(e.args) <= n {
		return d, nil
	}

	return doGetIntArgDefault(e.args[n], d)
}

func getIntNamedOrPasArgDefault(e *expr, k string, n int, d int) (int, error) {
	if len(e.args) <= n {
		return d, nil
	}

	if a := getNamedArg(e.args, k); a != nil {
		return doGetIntArgDefault(a, d)
	}

	return getIntArgDefault(e, n, d)
}

func doGetIntArgDefault(e *expr, d int) (int, error) {
	if e.etype != etConst && e.etype != etKV {
		return 0, ErrBadType
	}

	v := int(e.val)
	if v == 0 {
		return d, nil
	}

	return v, nil
}

func getBoolNamedOrPosArgDefault(e *expr, k string, n int, b bool) (bool, error) {
	if len(e.args) <= n {
		return b, nil
	}

	if a := getNamedArg(e.args, k); a != nil {
		return doGetBoolArgDefault(a, b)
	}

	return getBoolArgDefault(e, n, b)
}

func getBoolArgDefault(e *expr, n int, b bool) (bool, error) {
	if len(e.args) <= n {
		return b, nil
	}
	return doGetBoolArgDefault(e.args[n], b)
}

func doGetBoolArgDefault(e *expr, b bool) (bool, error) {
	if e.etype != etName && e.etype != etKV {
		return false, ErrBadType
	}

	// names go into 'target'
	switch e.target {
	case "False", "false":
		return false, nil
	case "True", "true":
		return true, nil
	}

	return false, ErrBadType
}

func getSeriesArg(arg *expr, from, until int32, values map[metricRequest][]*metricData) ([]*metricData, error) {

	if arg.etype != etName && arg.etype != etFunc {
		return nil, ErrMissingTimeseries
	}
	a := evalExpr(arg, from, until, values)

	if len(a) == 0 {
		return nil, ErrMissingTimeseries
	}

	return a, nil
}

func getSeriesArgs(e []*expr, from, until int32, values map[metricRequest][]*metricData) ([]*metricData, error) {

	var args []*metricData

	for _, arg := range e {
		a, err := getSeriesArg(arg, from, until, values)
		if err != nil {
			return nil, err
		}
		args = append(args, a...)
	}

	if len(args) == 0 {
		return nil, ErrMissingTimeseries
	}

	return args, nil
}

func getNamedArg(e []*expr, name string) *expr {
	for _, a := range e {
		if a.etype == etKV && a.key == name {
			return a
		}
	}

	return nil
}

func evalExpr(e *expr, from, until int32, values map[metricRequest][]*metricData) []*metricData {

	switch e.etype {
	case etName:
		return values[metricRequest{metric: e.target, from: from, until: until}]
	case etConst:
		p := metricData{FetchResponse: pb.FetchResponse{Name: proto.String(e.target), Values: []float64{e.val}}}
		return []*metricData{&p}
	}

	// evaluate the function

	// all functions have arguments -- check we do too
	if len(e.args) == 0 {
		return nil
	}

	switch e.target {
	case "absolute": // absolute(seriesList)
		return forEachSeriesDo(e, from, until, values, func(a *metricData, r *metricData) *metricData {
			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = math.Abs(v)
			}
			return r
		})

	case "alias": // alias(seriesList, newName)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		alias, err := getStringArg(e, 1)
		if err != nil {
			return nil
		}

		r := *arg[0]
		r.Name = proto.String(alias)
		return []*metricData{&r}

	case "aliasByMetric": // aliasByMetric(seriesList)
		return forEachSeriesDo(e, from, until, values, func(a *metricData, r *metricData) *metricData {
			metric := extractMetric(a.GetName())
			part := strings.Split(metric, ".")
			r.Name = proto.String(part[len(part)-1])
			r.Values = a.Values
			r.IsAbsent = a.IsAbsent
			return r
		})

	case "aliasByNode": // aliasByNode(seriesList, *nodes)
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		fields, err := getIntArgs(e, 1)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range args {

			metric := extractMetric(a.GetName())
			nodes := strings.Split(metric, ".")

			var name []string
			for _, f := range fields {
				if f < 0 {
					f += len(nodes)
				}
				if f >= len(nodes) || f < 0 {
					continue
				}
				name = append(name, nodes[f])
			}

			r := *a
			r.Name = proto.String(strings.Join(name, "."))
			results = append(results, &r)
		}

		return results

	case "aliasSub": // aliasSub(seriesList, search, replace)
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		search, err := getStringArg(e, 1)
		if err != nil {
			return nil
		}

		replace, err := getStringArg(e, 2)
		if err != nil {
			return nil
		}

		re, err := regexp.Compile(search)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range args {
			metric := extractMetric(a.GetName())

			r := *a
			r.Name = proto.String(re.ReplaceAllString(metric, replace))
			results = append(results, &r)
		}

		return results

	case "asPercent": // asPercent(seriesList, total=None)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		var getTotal func(i int) float64
		var formatName func(a *metricData) string

		if len(e.args) == 1 {
			getTotal = func(i int) float64 {
				var t float64
				var atLeastOne bool
				for _, a := range arg {
					if a.IsAbsent[i] {
						continue
					}
					atLeastOne = true
					t += a.Values[i]
				}
				if !atLeastOne {
					t = math.NaN()
				}

				return t
			}
			formatName = func(a *metricData) string {
				return fmt.Sprintf("asPercent(%s)", a.GetName())
			}
		} else if len(e.args) == 2 && e.args[1].etype == etConst {
			total, err := getFloatArg(e, 1)
			if err != nil {
				return nil
			}
			getTotal = func(i int) float64 { return total }
			formatName = func(a *metricData) string {
				return fmt.Sprintf("asPercent(%s,%g)", a.GetName(), total)
			}
		} else if len(e.args) == 2 && (e.args[1].etype == etName || e.args[1].etype == etFunc) {
			total, err := getSeriesArg(e.args[1], from, until, values)
			if err != nil || len(total) != 1 {
				return nil
			}
			getTotal = func(i int) float64 {
				if total[0].IsAbsent[i] {
					return math.NaN()
				}
				return total[0].Values[i]
			}
			var totalString string
			if e.args[1].etype == etName {
				totalString = e.args[1].target
			} else {
				totalString = fmt.Sprintf("%s(%s)", e.args[1].target, e.args[1].argString)
			}
			formatName = func(a *metricData) string {
				return fmt.Sprintf("asPercent(%s,%s)", a.GetName(), totalString)
			}
		} else {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(formatName(a))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))
			results = append(results, &r)
		}

		for i := range results[0].Values {

			total := getTotal(i)

			for j := range results {
				r := results[j]
				a := arg[j]

				if a.IsAbsent[i] || math.IsNaN(total) || total == 0 {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}

				r.Values[i] = (a.Values[i] / total) * 100
			}
		}
		return results

	case "avg", "averageSeries": // averageSeries(*seriesLists)
		args, err := getSeriesArgs(e.args, from, until, values)
		if err != nil {
			return nil
		}

		e.target = "averageSeries"
		return aggregateSeries(e, args, func(values []float64) float64 {
			sum := 0.0
			for _, value := range values {
				sum += value
			}
			return sum / float64(len(values))
		})

	case "averageSeriesWithWildcards": // averageSeriesWithWildcards(seriesLIst, *position)
		/* TODO(dgryski): make sure the arrays are all the same 'size'
		   (duplicated from sumSeriesWithWildcards because of similar logic but aggregation) */
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		fields, err := getIntArgs(e, 1)
		if err != nil {
			return nil
		}

		var results []*metricData

		groups := make(map[string][]*metricData)

		for _, a := range args {
			metric := extractMetric(a.GetName())
			nodes := strings.Split(metric, ".")
			var s []string
			// Yes, this is O(n^2), but len(nodes) < 10 and len(fields) < 3
			// Iterating an int slice is faster than a map for n ~ 30
			// http://www.antoine.im/posts/someone_is_wrong_on_the_internet
			for i, n := range nodes {
				if !contains(fields, i) {
					s = append(s, n)
				}
			}

			node := strings.Join(s, ".")

			groups[node] = append(groups[node], a)
		}

		for series, args := range groups {
			r := *args[0]
			r.Name = proto.String(fmt.Sprintf("averageSeriesWithWildcards(%s)", series))
			r.Values = make([]float64, len(args[0].Values))
			r.IsAbsent = make([]bool, len(args[0].Values))

			length := make([]float64, len(args[0].Values))
			atLeastOne := make([]bool, len(args[0].Values))
			for _, arg := range args {
				for i, v := range arg.Values {
					if arg.IsAbsent[i] {
						continue
					}
					atLeastOne[i] = true
					length[i]++
					r.Values[i] += v
				}
			}

			for i, v := range atLeastOne {
				if v {
					r.Values[i] = r.Values[i] / length[i]
				} else {
					r.IsAbsent[i] = true
				}
			}

			results = append(results, &r)
		}
		return results

	case "averageAbove", "averageBelow", "currentAbove", "currentBelow", "maximumAbove", "maximumBelow", "minimumAbove", "minimumBelow": // averageAbove(seriesList, n), averageBelow(seriesList, n), currentAbove(seriesList, n), currentBelow(seriesList, n), maximumAbove(seriesList, n), maximumBelow(seriesList, n), minimumAbove(seriesList, n), minimumBelow
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		n, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}

		isAbove := strings.HasSuffix(e.target, "Above")
		isInclusive := true
		var compute func([]float64, []bool) float64
		switch {
		case strings.HasPrefix(e.target, "average"):
			compute = avgValue
		case strings.HasPrefix(e.target, "current"):
			compute = currentValue
		case strings.HasPrefix(e.target, "maximum"):
			compute = maxValue
			isInclusive = false
		case strings.HasPrefix(e.target, "minimum"):
			compute = minValue
			isInclusive = false
		}
		var results []*metricData
		for _, a := range args {
			value := compute(a.Values, a.IsAbsent)
			if isAbove {
				if isInclusive {
					if value >= n {
						results = append(results, a)
					}
				} else {
					if value > n {
						results = append(results, a)
					}
				}
			} else {
				if value <= n {
					results = append(results, a)
				}
			}
		}

		return results

	case "derivative": // derivative(seriesList)
		return forEachSeriesDo(e, from, until, values, func(a *metricData, r *metricData) *metricData {
			prev := a.Values[0]
			for i, v := range a.Values {
				if i == 0 || a.IsAbsent[i] {
					r.IsAbsent[i] = true
					continue
				}

				r.Values[i] = v - prev
				prev = v
			}
			return r
		})

	case "diffSeries": // diffSeries(*seriesLists)
		if len(e.args) < 2 {
			return nil
		}

		minuend, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		subtrahends, err := getSeriesArgs(e.args[1:], from, until, values)
		if err != nil {
			return nil
		}

		// FIXME: need more error checking on minuend, subtrahends here
		r := *minuend[0]
		r.Name = proto.String(fmt.Sprintf("diffSeries(%s)", e.argString))
		r.Values = make([]float64, len(minuend[0].Values))
		r.IsAbsent = make([]bool, len(minuend[0].Values))

		for i, v := range minuend[0].Values {

			if minuend[0].IsAbsent[i] {
				r.IsAbsent[i] = true
				continue
			}

			var sub float64
			for _, s := range subtrahends {
				if s.IsAbsent[i] {
					continue
				}
				sub += s.Values[i]
			}

			r.Values[i] = v - sub
		}
		return []*metricData{&r}

	case "divideSeries": // divideSeries(dividendSeriesList, divisorSeriesList)
		if len(e.args) < 1 {
			return nil
		}

		numerators, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		var numerator, denominator *metricData
		if len(numerators) == 1 && len(e.args) == 2 {
			numerator = numerators[0]
			denominators, err := getSeriesArg(e.args[1], from, until, values)
			if err != nil || len(denominators) != 1 {
				return nil
			}

			denominator = denominators[0]
		} else if len(numerators) == 2 && len(e.args) == 1 {
			numerator = numerators[0]
			denominator = numerators[1]
		} else {
			return nil
		}

		if numerator.GetStepTime() != denominator.GetStepTime() || len(numerator.Values) != len(denominator.Values) {
			return nil
		}

		r := *numerator
		r.Name = proto.String(fmt.Sprintf("divideSeries(%s)", e.argString))
		r.Values = make([]float64, len(numerator.Values))
		r.IsAbsent = make([]bool, len(numerator.Values))

		for i, v := range numerator.Values {

			if numerator.IsAbsent[i] || denominator.IsAbsent[i] || denominator.Values[i] == 0 {
				r.IsAbsent[i] = true
				continue
			}

			r.Values[i] = v / denominator.Values[i]
		}
		return []*metricData{&r}

	case "multiplySeries": // multiplySeries(factorsSeriesList)
		r := metricData{
			FetchResponse: pb.FetchResponse{
				Name:      proto.String(fmt.Sprintf("multiplySeries(%s)", e.argString)),
				StartTime: &from,
				StopTime:  &until,
			},
		}
		for _, arg := range e.args {
			series, err := getSeriesArg(arg, from, until, values)
			if err != nil {
				return nil
			}

			if r.Values == nil {
				r.IsAbsent = make([]bool, len(series[0].IsAbsent))
				r.StepTime = series[0].StepTime
				r.Values = make([]float64, len(series[0].Values))
				copy(r.IsAbsent, series[0].IsAbsent)
				copy(r.Values, series[0].Values)
				series = series[1:]
			}

			for _, factor := range series {
				for i, v := range r.Values {
					if r.IsAbsent[i] || factor.IsAbsent[i] {
						r.IsAbsent[i] = true
						r.Values[i] = math.NaN()
						continue
					}

					r.Values[i] = v * factor.Values[i]
				}
			}
		}

		return []*metricData{&r}

	case "ewma", "exponentialWeightedMovingAverage": // ewma(seriesList, alpha)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		alpha, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}

		e.target = "ewma"

		// ugh, forEachSeriesDo does not handle arguments properly
		var results []*metricData
		for _, a := range arg {
			name := fmt.Sprintf("ewma(%s,%v)", a.GetName(), alpha)

			r := *a
			r.Name = proto.String(name)
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			ewma := onlinestats.NewExpWeight(alpha)

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.IsAbsent[i] = true
					continue
				}

				ewma.Push(v)
				r.Values[i] = ewma.Mean()
			}
			results = append(results, &r)
		}
		return results

	case "exclude": // exclude(seriesList, pattern)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		pat, err := getStringArg(e, 1)
		if err != nil {
			return nil
		}

		patre, err := regexp.Compile(pat)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			if !patre.MatchString(a.GetName()) {
				results = append(results, a)
			}
		}

		return results

	case "grep": // grep(seriesList, pattern)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		pat, err := getStringArg(e, 1)
		if err != nil {
			return nil
		}

		patre, err := regexp.Compile(pat)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			if patre.MatchString(a.GetName()) {
				results = append(results, a)
			}
		}

		return results

	case "group": // group(*seriesLists)
		args, err := getSeriesArgs(e.args, from, until, values)
		if err != nil {
			return nil
		}

		return args

	case "groupByNode": // groupByNode(seriesList, nodeNum, callback)
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		field, err := getIntArg(e, 1)
		if err != nil {
			return nil
		}

		callback, err := getStringArg(e, 2)
		if err != nil {
			return nil
		}

		var results []*metricData

		groups := make(map[string][]*metricData)

		for _, a := range args {

			metric := extractMetric(a.GetName())
			nodes := strings.Split(metric, ".")
			node := nodes[field]

			groups[node] = append(groups[node], a)
		}

		for k, v := range groups {
			k := k // k's reference is used later, so it's important to make it unique per loop

			// create a stub context to evaluate the callback in
			nexpr, _, err := parseExpr(fmt.Sprintf("%s(%s)", callback, k))
			if err != nil {
				return nil
			}

			nvalues := map[metricRequest][]*metricData{
				metricRequest{k, from, until}: v,
			}

			r := evalExpr(nexpr, from, until, nvalues)
			if r != nil {
				r[0].Name = &k
				results = append(results, r...)
			}
		}

		return results

	case "isNonNull", "isNotNull": // isNonNull(seriesList), isNotNull(seriesList)

		e.target = "isNonNull"

		return forEachSeriesDo(e, from, until, values, func(a *metricData, r *metricData) *metricData {
			for i := range a.Values {
				r.IsAbsent[i] = false
				if a.IsAbsent[i] {
					r.Values[i] = 0
				} else {
					r.Values[i] = 1
				}

			}
			return r
		})

	case "lowestAverage", "lowestCurrent": // lowestAverage(seriesList, n) , lowestCurrent(seriesList, n)

		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		n, err := getIntArg(e, 1)
		if err != nil {
			return nil
		}
		var results []*metricData

		// we have fewer arguments than we want result series
		if len(arg) < n {
			return arg
		}

		var mh metricHeap

		var compute func([]float64, []bool) float64

		switch e.target {
		case "lowestAverage":
			compute = avgValue
		case "lowestCurrent":
			compute = currentValue
		}

		for i, a := range arg {
			m := compute(a.Values, a.IsAbsent)
			heap.Push(&mh, metricHeapElement{idx: i, val: m})
		}

		results = make([]*metricData, n)

		// results should be ordered ascending
		for i := 0; i < n; i++ {
			v := heap.Pop(&mh).(metricHeapElement)
			results[i] = arg[v.idx]
		}

		return results

	case "highestAverage", "highestCurrent", "highestMax": // highestAverage(seriesList, n) , highestCurrent(seriesList, n), highestMax(seriesList, n)

		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		n, err := getIntArg(e, 1)
		if err != nil {
			return nil
		}
		var results []*metricData

		// we have fewer arguments than we want result series
		if len(arg) < n {
			return arg
		}

		var mh metricHeap

		var compute func([]float64, []bool) float64

		switch e.target {
		case "highestMax":
			compute = maxValue
		case "highestAverage":
			compute = avgValue
		case "highestCurrent":
			compute = currentValue
		}

		for i, a := range arg {
			m := compute(a.Values, a.IsAbsent)
			if math.IsNaN(m) {
				continue
			}

			if len(mh) < n {
				heap.Push(&mh, metricHeapElement{idx: i, val: m})
				continue
			}
			// m is bigger than smallest max found so far
			if mh[0].val < m {
				mh[0].val = m
				mh[0].idx = i
				heap.Fix(&mh, 0)
			}
		}

		results = make([]*metricData, n)

		// results should be ordered ascending
		for len(mh) > 0 {
			v := heap.Pop(&mh).(metricHeapElement)
			results[len(mh)] = arg[v.idx]
		}

		return results

	case "hitcount": // hitcount(seriesList, intervalString, alignToInterval=False)
		// TODO(dgryski): make sure the arrays are all the same 'size'
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		bucketSize, err := getIntervalArg(e, 1, 1)
		if err != nil {
			return nil
		}

		alignToInterval, err := getBoolNamedOrPosArgDefault(e, "alignToInterval", 2, false)
		if err != nil {
			return nil
		}

		start := args[0].GetStartTime()
		stop := args[0].GetStopTime()
		if alignToInterval {
			start = alignStartToInterval(start, stop, bucketSize)
		}

		buckets := getBuckets(start, stop, bucketSize)
		results := make([]*metricData, 0, len(args))
		for _, arg := range args {

			var name string
			switch len(e.args) {
			case 2:
				name = fmt.Sprintf("hitcount(%s,'%s')", arg.GetName(), e.args[1].valStr)
			case 3:
				name = fmt.Sprintf("hitcount(%s,'%s',%s)", arg.GetName(), e.args[1].valStr, e.args[2].target)
			}

			r := metricData{FetchResponse: pb.FetchResponse{
				Name:      proto.String(name),
				Values:    make([]float64, buckets, buckets+1),
				IsAbsent:  make([]bool, buckets, buckets+1),
				StepTime:  proto.Int32(bucketSize),
				StartTime: proto.Int32(start),
				StopTime:  proto.Int32(stop),
			}}

			bucketEnd := start + bucketSize
			t := arg.GetStartTime()
			ridx := 0
			var count float64
			bucketItems := 0
			for i, v := range arg.Values {
				bucketItems++
				if !arg.IsAbsent[i] {
					if math.IsNaN(count) {
						count = 0
					}

					count += v * float64(arg.GetStepTime())
				}

				t += arg.GetStepTime()

				if t >= stop {
					break
				}

				if t >= bucketEnd {
					if math.IsNaN(count) {
						r.Values[ridx] = 0
						r.IsAbsent[ridx] = true
					} else {
						r.Values[ridx] = count
					}

					ridx++
					bucketEnd += bucketSize
					count = math.NaN()
					bucketItems = 0
				}
			}

			// remaining values
			if bucketItems > 0 {
				if math.IsNaN(count) {
					r.Values[ridx] = 0
					r.IsAbsent[ridx] = true
				} else {
					r.Values[ridx] = count
				}
			}

			results = append(results, &r)
		}
		return results
	case "integral": // integral(seriesList)
		return forEachSeriesDo(e, from, until, values, func(a *metricData, r *metricData) *metricData {
			current := 0.0
			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				current += v
				r.Values[i] = current
			}
			return r
		})

	case "invert": // invert(seriesList)
		return forEachSeriesDo(e, from, until, values, func(a *metricData, r *metricData) *metricData {
			for i, v := range a.Values {
				if a.IsAbsent[i] || v == 0 {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = 1 / v
			}
			return r
		})

	case "keepLastValue": // keepLastValue(seriesList, limit=inf)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		keep, err := getIntNamedOrPasArgDefault(e, "limit", 1, -1)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			var name string
			if len(e.args) == 1 {
				name = fmt.Sprintf("keepLastValue(%s)", a.GetName())
			} else {
				name = fmt.Sprintf("keepLastValue(%s,%d)", a.GetName(), keep)
			}

			r := *a
			r.Name = proto.String(name)
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			prev := math.NaN()
			missing := 0

			for i, v := range a.Values {
				if a.IsAbsent[i] {

					if (keep < 0 || missing < keep) && !math.IsNaN(prev) {
						r.Values[i] = prev
						missing++
					} else {
						r.IsAbsent[i] = true
					}

					continue
				}
				missing = 0
				prev = v
				r.Values[i] = v
			}
			results = append(results, &r)
		}
		return results

	case "changed": // changed(SeriesList)
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		var result []*metricData
		for _, a := range args {
			r := *a
			r.Name = proto.String(fmt.Sprintf("%s(%s)", e.target, a.GetName()))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			prev := math.NaN()
			for i, v := range a.Values {
				if math.IsNaN(prev) {
					prev = v
					r.Values[i] = 0
				} else if !math.IsNaN(v) && prev != v {
					r.Values[i] = 1
					prev = v
				} else {
					r.Values[i] = 0
				}
			}
			result = append(result, &r)
		}
		return result

	case "kolmogorovSmirnovTest2", "ksTest2": // ksTest2(series, series, points|"interval")
		arg1, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		arg2, err := getSeriesArg(e.args[1], from, until, values)
		if err != nil {
			return nil
		}

		if len(arg1) != 1 || len(arg2) != 1 {
			// no wildcards allowed
			return nil
		}

		a1 := arg1[0]
		a2 := arg2[0]

		windowSize, err := getIntArg(e, 2)
		if err != nil {
			return nil
		}

		w1 := &Windowed{data: make([]float64, windowSize)}
		w2 := &Windowed{data: make([]float64, windowSize)}

		r := *a1
		r.Name = proto.String(fmt.Sprintf("kolmogorovSmirnovTest2(%s,%s,%d)", a1.GetName(), a2.GetName(), windowSize))
		r.Values = make([]float64, len(a1.Values))
		r.IsAbsent = make([]bool, len(a1.Values))
		r.StartTime = proto.Int32(from)
		r.StopTime = proto.Int32(until)

		d1 := make([]float64, windowSize)
		d2 := make([]float64, windowSize)

		for i, v1 := range a1.Values {
			v2 := a2.Values[i]
			if a1.IsAbsent[i] || a2.IsAbsent[i] {
				// make sure missing values are ignored
				v1 = math.NaN()
				v2 = math.NaN()
			}
			w1.Push(v1)
			w2.Push(v2)

			if i >= windowSize {
				// need a copy here because KS is destructive
				copy(d1, w1.data)
				copy(d2, w2.data)
				r.Values[i] = onlinestats.KS(d1, d2)
			} else {
				r.Values[i] = 0
				r.IsAbsent[i] = true
			}
		}
		return []*metricData{&r}

	case "limit": // limit(seriesList, n)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		limit, err := getIntArg(e, 1) // get limit
		if err != nil {
			return nil
		}

		if limit >= len(arg) {
			return arg
		}

		return arg[:limit]

	case "logarithm", "log": // logarithm(seriesList, base=10)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		base, err := getIntNamedOrPasArgDefault(e, "base", 1, 10)
		if err != nil {
			return nil
		}
		baseLog := math.Log(float64(base))

		var results []*metricData

		for _, a := range arg {

			var name string
			if len(e.args) == 1 {
				name = fmt.Sprintf("logarithm(%s)", a.GetName())
			} else {
				name = fmt.Sprintf("logarithm(%s,%d)", a.GetName(), base)
			}

			r := *a
			r.Name = proto.String(name)
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = math.Log(v) / baseLog
			}
			results = append(results, &r)
		}
		return results

	case "maxSeries": // maxSeries(*seriesLists)
		args, err := getSeriesArgs(e.args, from, until, values)
		if err != nil {
			return nil
		}

		return aggregateSeries(e, args, func(values []float64) float64 {
			max := math.Inf(-1)
			for _, value := range values {
				if value > max {
					max = value
				}
			}
			return max
		})

	case "minSeries": // minSeries(*seriesLists)
		args, err := getSeriesArgs(e.args, from, until, values)
		if err != nil {
			return nil
		}

		return aggregateSeries(e, args, func(values []float64) float64 {
			min := math.Inf(1)
			for _, value := range values {
				if value < min {
					min = value
				}
			}
			return min
		})

	case "mostDeviant": // mostDeviant(n, seriesList)
		n, err := getIntArg(e, 0)
		if err != nil {
			return nil
		}

		args, err := getSeriesArg(e.args[1], from, until, values)
		if err != nil {
			return nil
		}

		var mh metricHeap

		for index, arg := range args {
			variance := varianceValue(arg.Values, arg.IsAbsent)
			if math.IsNaN(variance) {
				continue
			}

			if len(mh) < n {
				heap.Push(&mh, metricHeapElement{idx: index, val: variance})
				continue
			}

			if variance > mh[0].val {
				mh[0].idx = index
				mh[0].val = variance
				heap.Fix(&mh, 0)
			}
		}

		results := make([]*metricData, n)

		for len(mh) > 0 {
			v := heap.Pop(&mh).(metricHeapElement)
			results[len(mh)] = args[v.idx]
		}

		return results

	case "movingAverage": // movingAverage(seriesList, windowSize)
		var n int
		var err error

		var scaleByStep bool

		switch e.args[1].etype {
		case etConst:
			n, err = getIntArg(e, 1)
		case etString:
			var n32 int32
			n32, err = getIntervalArg(e, 1, 1)
			n = int(n32)
			scaleByStep = true
		default:
			err = ErrBadType
		}
		if err != nil {
			return nil
		}

		windowSize := n

		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		if scaleByStep {
			windowSize /= int(arg[0].GetStepTime())
		}

		var result []*metricData

		for _, a := range arg {
			w := &Windowed{data: make([]float64, windowSize)}

			r := *a
			r.Name = proto.String(fmt.Sprintf("movingAverage(%s,%d)", a.GetName(), windowSize))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))
			r.StartTime = proto.Int32(from)
			r.StopTime = proto.Int32(until)

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					// make sure missing values are ignored
					v = math.NaN()
				}
				r.Values[i] = w.Mean()
				w.Push(v)
				if i < windowSize || math.IsNaN(r.Values[i]) {
					r.Values[i] = 0
					r.IsAbsent[i] = true
				}
			}
			result = append(result, &r)
		}
		return result

	case "movingMedian": // movingMedian(seriesList, windowSize)
		var n int
		var err error

		var scaleByStep bool

		switch e.args[1].etype {
		case etConst:
			n, err = getIntArg(e, 1)
		case etString:
			var n32 int32
			n32, err = getIntervalArg(e, 1, 1)
			n = int(n32)
			scaleByStep = true
		default:
			err = ErrBadType
		}
		if err != nil {
			return nil
		}

		windowSize := n

		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		if scaleByStep {
			windowSize /= int(arg[0].GetStepTime())
		}

		var result []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("movingMedian(%s,%d)", a.GetName(), windowSize))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))
			r.StartTime = proto.Int32(from)
			r.StopTime = proto.Int32(until)

			data := movingmedian.NewMovingMedian(windowSize)

			for i, v := range a.Values {
				r.Values[i] = math.NaN()
				if a.IsAbsent[i] {
					data.Push(math.NaN())
				} else {
					data.Push(v)
				}
				if i >= (windowSize - 1) {
					r.Values[i] = data.Median()
				}
				if math.IsNaN(r.Values[i]) {
					r.IsAbsent[i] = true
				}
			}
			result = append(result, &r)
		}
		return result

	case "nonNegativeDerivative": // nonNegativeDerivative(seriesList, maxValue=None)
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		maxValue, err := getFloatNamedOrPosArgDefault(e, "maxValue", 1, math.NaN())
		if err != nil {
			return nil
		}

		var result []*metricData
		for _, a := range args {
			var name string
			if len(e.args) == 1 {
				name = fmt.Sprintf("nonNegativeDerivative(%s)", a.GetName())
			} else {
				name = fmt.Sprintf("nonNegativeDerivative(%s,%g)", a.GetName(), maxValue)
			}

			r := *a
			r.Name = proto.String(name)
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			prev := a.Values[0]
			for i, v := range a.Values {
				if i == 0 || a.IsAbsent[i] || a.IsAbsent[i-1] {
					r.IsAbsent[i] = true
					prev = v
					continue
				}
				diff := v - prev
				if diff >= 0 {
					r.Values[i] = diff
				} else if !math.IsNaN(maxValue) && maxValue >= v {
					r.Values[i] = ((maxValue - prev) + v + 1)
				} else {
					r.Values[i] = 0
					r.IsAbsent[i] = true
				}
				prev = v
			}
			result = append(result, &r)
		}
		return result

	case "perSecond": // perSecond(seriesList, maxValue=None)
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		maxValue, err := getFloatArgDefault(e, 1, math.NaN())
		if err != nil {
			return nil
		}

		var result []*metricData
		for _, a := range args {
			r := *a
			if len(e.args) == 1 {
				r.Name = proto.String(fmt.Sprintf("%s(%s)", e.target, a.GetName()))
			} else {
				r.Name = proto.String(fmt.Sprintf("%s(%s,%g)", e.target, a.GetName(), maxValue))
			}
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			prev := a.Values[0]
			for i, v := range a.Values {
				if i == 0 || a.IsAbsent[i] || a.IsAbsent[i-1] {
					r.IsAbsent[i] = true
					prev = v
					continue
				}
				diff := v - prev
				if diff >= 0 {
					r.Values[i] = diff / float64(a.GetStepTime())
				} else if !math.IsNaN(maxValue) && maxValue >= v {
					r.Values[i] = ((maxValue - prev) + v + 1/float64(a.GetStepTime()))
				} else {
					r.Values[i] = 0
					r.IsAbsent[i] = true
				}
				prev = v
			}
			result = append(result, &r)
		}
		return result

	case "nPercentile": // nPercentile(seriesList, n)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		percent, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}

		var results []*metricData
		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("nPercentile(%s,%g)", a.GetName(), percent))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			var values []float64
			for i, v := range a.IsAbsent {
				if !v {
					values = append(values, a.Values[i])
				}
			}

			value := percentile(values, percent, true)
			for i := range r.Values {
				r.Values[i] = value
			}

			results = append(results, &r)
		}
		return results

	case "pearson": // pearson(series, series, windowSize)
		arg1, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		arg2, err := getSeriesArg(e.args[1], from, until, values)
		if err != nil {
			return nil
		}

		if len(arg1) != 1 || len(arg2) != 1 {
			// must be single series
			return nil
		}

		a1 := arg1[0]
		a2 := arg2[0]

		windowSize, err := getIntArg(e, 2)
		if err != nil {
			return nil
		}

		w1 := &Windowed{data: make([]float64, windowSize)}
		w2 := &Windowed{data: make([]float64, windowSize)}

		r := *a1
		r.Name = proto.String(fmt.Sprintf("pearson(%s,%s,%d)", a1.GetName(), a2.GetName(), windowSize))
		r.Values = make([]float64, len(a1.Values))
		r.IsAbsent = make([]bool, len(a1.Values))
		r.StartTime = proto.Int32(from)
		r.StopTime = proto.Int32(until)

		for i, v1 := range a1.Values {
			v2 := a2.Values[i]
			if a1.IsAbsent[i] || a2.IsAbsent[i] {
				// ignore if either is missing
				v1 = math.NaN()
				v2 = math.NaN()
			}
			w1.Push(v1)
			w2.Push(v2)
			if i >= windowSize-1 {
				r.Values[i] = onlinestats.Pearson(w1.data, w2.data)
			} else {
				r.Values[i] = 0
				r.IsAbsent[i] = true
			}
		}

		return []*metricData{&r}

	case "pearsonClosest": // pearsonClosest(series, seriesList, n, direction=abs)
		ref, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		if len(ref) != 1 {
			// TODO(nnuss) error("First argument must be single reference series")
			return nil
		}

		compare, err := getSeriesArg(e.args[1], from, until, values)
		if err != nil {
			return nil
		}

		n, err := getIntArg(e, 2)
		if err != nil {
			return nil
		}

		direction, err := getStringNamedOrPosArgDefault(e, "direction", 3, "abs")
		if err != nil && len(e.args) > 3 {
			return nil
		}
		if direction != "pos" && direction != "neg" && direction != "abs" {
			// TODO(nnuss) error("pearsonClosest( _ , _ , direction=abs ) : direction must be one of { 'pos', 'neg', 'abs' }")
			return nil
		}

		// NOTE: if direction == "abs" && len(compare) <= n : we'll still do the work to rank them

		refValues := make([]float64, len(ref[0].Values))
		copy(refValues, ref[0].Values)
		for i, v := range ref[0].IsAbsent {
			if v == true {
				refValues[i] = math.NaN()
			}
		}

		var mh metricHeap

		for index, a := range compare {
			compareValues := make([]float64, len(a.Values))
			copy(compareValues, a.Values)
			if len(refValues) != len(compareValues) {
				// Pearson will panic if arrays are not equal length; skip
				continue
			}
			for i, v := range a.IsAbsent {
				if v == true {
					compareValues[i] = math.NaN()
				}
			}
			value := onlinestats.Pearson(refValues, compareValues)
			// Standardize the value so sort ASC will have strongest correlation first
			switch {
			case math.IsNaN(value):
				// special case of at least one series containing all zeros which leads to div-by-zero in Pearson
				continue
			case direction == "abs":
				value = math.Abs(value) * -1
			case direction == "pos" && value >= 0:
				value = value * -1
			case direction == "neg" && value <= 0:
			default:
				continue
			}
			heap.Push(&mh, metricHeapElement{idx: index, val: value})
		}

		if n > len(mh) {
			n = len(mh)
		}
		results := make([]*metricData, n)
		for i := range results {
			v := heap.Pop(&mh).(metricHeapElement)
			results[i] = compare[v.idx]
		}

		return results

	case "offset": // offset(seriesList,factor)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		factor, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}
		var results []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("offset(%s,%g)", a.GetName(), factor))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = v + factor
			}
			results = append(results, &r)
		}
		return results

	case "offsetToZero": // offsetToZero(seriesList)
		return forEachSeriesDo(e, from, until, values, func(a *metricData, r *metricData) *metricData {
			minimum := math.Inf(1)
			for i, v := range a.Values {
				if !a.IsAbsent[i] && v < minimum {
					minimum = v
				}
			}
			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = v - minimum
			}
			return r
		})

	case "scale": // scale(seriesList, factor)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		scale, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}
		var results []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("scale(%s,%g)", a.GetName(), scale))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = v * scale
			}
			results = append(results, &r)
		}
		return results

	case "scaleToSeconds": // scaleToSeconds(seriesList, seconds)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		seconds, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("scaleToSeconds(%s,%d)", a.GetName(), int(seconds)))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			factor := seconds / float64(a.GetStepTime())

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = v * factor
			}
			results = append(results, &r)
		}
		return results

	case "pow": // pow(seriesList,factor)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		factor, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}
		var results []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("pow(%s,%g)", a.GetName(), factor))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = math.Pow(v, factor)
			}
			results = append(results, &r)
		}
		return results

	case "sortByMaxima", "sortByMinima", "sortByTotal": // sortByMaxima(seriesList), sortByMinima(seriesList), sortByTotal(seriesList)
		original, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		arg := make([]*metricData, len(original))
		copy(arg, original)
		vals := make([]float64, len(arg))

		for i, a := range arg {
			switch e.target {
			case "sortByTotal":
				vals[i] = summarizeValues("sum", a.GetValues())
			case "sortByMaxima":
				vals[i] = summarizeValues("max", a.GetValues())
			case "sortByMinima":
				vals[i] = 1 / summarizeValues("min", a.GetValues())
			}
		}

		sort.Sort(byVals{vals: vals, series: arg})

		return arg

	case "sortByName": // sortByName(seriesList, natural=false)
		original, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		natSort, err := getBoolNamedOrPosArgDefault(e, "natural", 1, false)
		if err != nil {
			return nil
		}

		arg := make([]*metricData, len(original))
		copy(arg, original)
		if natSort {
			sort.Sort(ByNameNatural(arg))
		} else {
			sort.Sort(ByName(arg))
		}

		return arg

	case "stdev", "stddev": // stdev(seriesList, points, missingThreshold=0.1)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		points, err := getIntArg(e, 1)
		if err != nil {
			return nil
		}

		missingThreshold, err := getFloatArgDefault(e, 2, 0.1)
		if err != nil {
			return nil
		}

		minLen := int((1 - missingThreshold) * float64(points))

		var result []*metricData

		for _, a := range arg {
			w := &Windowed{data: make([]float64, points)}

			r := *a
			r.Name = proto.String(fmt.Sprintf("stdev(%s,%d)", a.GetName(), points))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					// make sure missing values are ignored
					v = math.NaN()
				}
				w.Push(v)
				r.Values[i] = w.Stdev()
				if math.IsNaN(r.Values[i]) || (i >= minLen && w.Len() < minLen) {
					r.Values[i] = 0
					r.IsAbsent[i] = true
				}
			}
			result = append(result, &r)
		}
		return result

	case "sum", "sumSeries": // sumSeries(*seriesLists)
		// TODO(dgryski): make sure the arrays are all the same 'size'
		args, err := getSeriesArgs(e.args, from, until, values)
		if err != nil {
			return nil
		}

		e.target = "sumSeries"
		return aggregateSeries(e, args, func(values []float64) float64 {
			sum := 0.0
			for _, value := range values {
				sum += value
			}
			return sum
		})

	case "sumSeriesWithWildcards": // sumSeriesWithWildcards(seriesList, *position)
		// TODO(dgryski): make sure the arrays are all the same 'size'
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		fields, err := getIntArgs(e, 1)
		if err != nil {
			return nil
		}

		var results []*metricData

		groups := make(map[string][]*metricData)

		for _, a := range args {
			metric := extractMetric(a.GetName())
			nodes := strings.Split(metric, ".")
			var s []string
			// Yes, this is O(n^2), but len(nodes) < 10 and len(fields) < 3
			// Iterating an int slice is faster than a map for n ~ 30
			// http://www.antoine.im/posts/someone_is_wrong_on_the_internet
			for i, n := range nodes {
				if !contains(fields, i) {
					s = append(s, n)
				}
			}

			node := strings.Join(s, ".")

			groups[node] = append(groups[node], a)
		}

		for series, args := range groups {
			r := *args[0]
			r.Name = proto.String(fmt.Sprintf("sumSeriesWithWildcards(%s)", series))
			r.Values = make([]float64, len(args[0].Values))
			r.IsAbsent = make([]bool, len(args[0].Values))

			atLeastOne := make([]bool, len(args[0].Values))
			for _, arg := range args {
				for i, v := range arg.Values {
					if arg.IsAbsent[i] {
						continue
					}
					atLeastOne[i] = true
					r.Values[i] += v
				}
			}

			for i, v := range atLeastOne {
				if !v {
					r.IsAbsent[i] = true
				}
			}

			results = append(results, &r)
		}
		return results

	case "percentileOfSeries": // percentileOfSeries(seriesList, n, interpolate=False)
		// TODO(dgryski): make sure the arrays are all the same 'size'
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		percent, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}

		interpolate, err := getBoolNamedOrPosArgDefault(e, "interpolate", 2, false)
		if err != nil {
			return nil
		}

		return aggregateSeries(e, args, func(values []float64) float64 {
			return percentile(values, percent, interpolate)
		})

	case "summarize": // summarize(seriesList, intervalString, func='sum', alignToFrom=False)
		// TODO(dgryski): make sure the arrays are all the same 'size'
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		bucketSize, err := getIntervalArg(e, 1, 1)
		if err != nil {
			return nil
		}

		summarizeFunction, err := getStringNamedOrPosArgDefault(e, "func", 2, "sum")
		if err != nil {
			return nil
		}

		alignToFrom, err := getBoolNamedOrPosArgDefault(e, "alignToFrom", 3, false)
		if err != nil {
			return nil
		}

		start := args[0].GetStartTime()
		stop := args[0].GetStopTime()
		if !alignToFrom {
			start, stop = alignToBucketSize(start, stop, bucketSize)
		}

		buckets := getBuckets(start, stop, bucketSize)
		results := make([]*metricData, 0, len(args))
		for _, arg := range args {

			var name string
			switch len(e.args) {
			case 2:
				name = fmt.Sprintf("summarize(%s,'%s')", arg.GetName(), e.args[1].valStr)
			case 3:
				name = fmt.Sprintf("summarize(%s,'%s','%s')", arg.GetName(), e.args[1].valStr, e.args[2].valStr)
			case 4:
				name = fmt.Sprintf("summarize(%s,'%s','%s',%s)", arg.GetName(), e.args[1].valStr, e.args[2].valStr, e.args[3].target)
			}

			r := metricData{FetchResponse: pb.FetchResponse{
				Name:      proto.String(name),
				Values:    make([]float64, buckets, buckets),
				IsAbsent:  make([]bool, buckets, buckets),
				StepTime:  proto.Int32(bucketSize),
				StartTime: proto.Int32(start),
				StopTime:  proto.Int32(stop),
			}}

			t := arg.GetStartTime() // unadjusted
			bucketEnd := start + bucketSize
			values := make([]float64, 0, bucketSize/arg.GetStepTime())
			ridx := 0
			bucketItems := 0
			for i, v := range arg.Values {
				bucketItems++
				if !arg.IsAbsent[i] {
					values = append(values, v)
				}

				t += arg.GetStepTime()

				if t >= stop {
					break
				}

				if t >= bucketEnd {
					rv := summarizeValues(summarizeFunction, values)

					if math.IsNaN(rv) {
						r.IsAbsent[ridx] = true
					}

					r.Values[ridx] = rv
					ridx++
					bucketEnd += bucketSize
					bucketItems = 0
					values = values[:0]
				}
			}

			// last partial bucket
			if bucketItems > 0 {
				rv := summarizeValues(summarizeFunction, values)
				if math.IsNaN(rv) {
					r.Values[ridx] = 0
					r.IsAbsent[ridx] = true
				} else {
					r.Values[ridx] = rv
					r.IsAbsent[ridx] = false
				}
			}

			results = append(results, &r)
		}
		return results

	case "timeShift": // timeShift(seriesList, timeShift, resetEnd=True)
		// FIXME(dgryski): support resetEnd=true

		offs, err := getIntervalArg(e, 1, -1)
		if err != nil {
			return nil
		}

		arg, err := getSeriesArg(e.args[0], from+offs, until+offs, values)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("timeShift(%s,'%d')", a.GetName(), offs))
			r.StartTime = proto.Int32(a.GetStartTime() - offs)
			r.StopTime = proto.Int32(a.GetStopTime() - offs)
			results = append(results, &r)
		}
		return results

	case "timeStack": // timeStack(seriesList, timeShiftUnit, timeShiftStart, timeShiftEnd)
		unit, err := getIntervalArg(e, 1, -1)
		if err != nil {
			return nil
		}

		start, err := getIntArg(e, 2)
		if err != nil {
			return nil
		}

		end, err := getIntArg(e, 3)
		if err != nil {
			return nil
		}

		var results []*metricData
		for i := int32(start); i < int32(end); i++ {
			offs := i * unit
			arg, err := getSeriesArg(e.args[0], from+offs, until+offs, values)
			if err != nil {
				return nil
			}

			for _, a := range arg {
				r := *a
				r.Name = proto.String(fmt.Sprintf("timeShift(%s,%d)", a.GetName(), offs))
				r.StartTime = proto.Int32(a.GetStartTime() - offs)
				r.StopTime = proto.Int32(a.GetStopTime() - offs)
				results = append(results, &r)
			}
		}

		return results

	case "transformNull": // transformNull(seriesList, default=0)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		defv, err := getFloatNamedOrPosArgDefault(e, "default", 1, 0)
		if err != nil {
			return nil
		}
		var results []*metricData

		for _, a := range arg {

			var name string
			if len(e.args) == 1 {
				name = fmt.Sprintf("transformNull(%s)", a.GetName())
			} else {
				name = fmt.Sprintf("transformNull(%s,%g)", a.GetName(), defv)
			}

			r := *a
			r.Name = proto.String(name)
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					v = defv
				}

				r.Values[i] = v
			}

			results = append(results, &r)
		}
		return results

	case "tukeyAbove", "tukeyBelow": // tukeyAbove(seriesList,basis,n,interval=0) , tukeyBelow(seriesList,basis,n,interval=0)

		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		basis, err := getFloatArg(e, 1)
		if err != nil || basis <= 0 {
			return nil
		}

		n, err := getIntArg(e, 2)
		if err != nil || n < 1 {
			return nil
		}

		var interval int
		if len(e.args) >= 4 {
			switch e.args[3].etype {
			case etConst:
				interval, err = getIntArg(e, 3)
			case etString:
				var i32 int32
				i32, err = getIntervalArg(e, 3, 1)
				interval = int(i32)
				interval /= int(arg[0].GetStepTime())
				// TODO(nnuss): make sure the arrays are all the same 'size'
			default:
				err = ErrBadType
			}
			if err != nil {
				return nil
			}
		}
		// TODO(nnuss): negative intervals

		// gather all the valid points
		var points []float64
		for _, a := range arg {
			for i, m := range a.Values {
				if a.IsAbsent[i] {
					continue
				}
				points = append(points, m)
			}
		}

		sort.Float64s(points)

		first := int(0.25 * float64(len(points)))
		third := int(0.75 * float64(len(points)))

		iqr := points[third] - points[first]

		max := points[third] + basis*iqr
		min := points[first] - basis*iqr

		isAbove := strings.HasSuffix(e.target, "Above")

		var mh metricHeap

		// count how many points are above the threshold
		for i, a := range arg {
			var outlier int
			for i, m := range a.Values {
				if a.IsAbsent[i] {
					continue
				}
				if isAbove {
					if m >= max {
						outlier++
					}
				} else {
					if m <= min {
						outlier++
					}
				}
			}

			// not even a single anomalous point -- ignore this metric
			if outlier == 0 {
				continue
			}

			if len(mh) < n {
				heap.Push(&mh, metricHeapElement{idx: i, val: float64(outlier)})
				continue
			}
			// current outlier count is is bigger than smallest max found so far
			foutlier := float64(outlier)
			if mh[0].val < foutlier {
				mh[0].val = foutlier
				mh[0].idx = i
				heap.Fix(&mh, 0)
			}
		}

		if len(mh) < n {
			n = len(mh)
		}
		results := make([]*metricData, n)
		// results should be ordered ascending
		for len(mh) > 0 {
			v := heap.Pop(&mh).(metricHeapElement)
			results[len(mh)] = arg[v.idx]
		}

		return results

	case "color": // color(seriesList, theColor)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		color, err := getStringArg(e, 1) // get color
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			r := *a
			r.color = color
			results = append(results, &r)
		}

		return results

	case "stacked": // stacked(seriesList, stackname="__DEFAULT__")
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		stackName, err := getStringNamedOrPosArgDefault(e, "stackname", 1, "__DEFAULT__")
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			r := *a
			r.stacked = true
			r.stackName = stackName
			results = append(results, &r)
		}

		return results

	case "alpha": // alpha(seriesList, theAlpha)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		alpha, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			r := *a
			r.alpha = alpha
			r.hasAlpha = true
			results = append(results, &r)
		}

		return results

	case "dashed", "drawAsInfinite", "secondYAxis":
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("%s(%s)", e.target, a.GetName()))

			switch e.target {
			case "dashed":
				r.dashed = true
			case "drawAsInfinite":
				r.drawAsInfinite = true
			case "secondYAxis":
				r.secondYAxis = true
			}

			results = append(results, &r)
		}
		return results

	case "constantLine":
		value, err := getFloatArg(e, 0)

		if err != nil {
			return nil
		}
		p := metricData{
			FetchResponse: pb.FetchResponse{
				Name:      proto.String(fmt.Sprintf("%g", value)),
				StartTime: proto.Int32(from),
				StopTime:  proto.Int32(until),
				StepTime:  proto.Int32(until - from),
				Values:    []float64{value, value},
				IsAbsent:  []bool{false, false},
			},
		}

		return []*metricData{&p}

	case "timeFunction", "time":
		name, err := getStringArg(e, 0)
		if err != nil {
			return nil
		}

		step_int, err := getIntArgDefault(e, 1, 60)
		if err != nil || step_int <= 0 {
			return nil
		}
		step := int32(step_int)

		// emulate the behavior of this Python code:
		//   while when < requestContext["endTime"]:
		//     values.append(time.mktime(when.timetuple()))
		//     when += delta

		values := make([]float64, (until-from-1+step)/step)
		value := from
		for i := 0; i < len(values); i++ {
			values[i] = float64(value)
			value += step
		}

		p := metricData{
			FetchResponse: pb.FetchResponse{
				Name:      proto.String(name),
				StartTime: proto.Int32(from),
				StopTime:  proto.Int32(until),
				StepTime:  proto.Int32(step),
				Values:    values,
				IsAbsent:  make([]bool, len(values)),
			},
		}

		return []*metricData{&p}

	case "threshold": // threshold(value, label=None, color=None)
		// XXX does not match graphite's signature

		value, err := getFloatArg(e, 0)

		if err != nil {
			return nil
		}

		name, err := getStringNamedOrPosArgDefault(e, "label", 1, fmt.Sprintf("%g", value))
		if err != nil {
			return nil
		}

		color, err := getStringNamedOrPosArgDefault(e, "color", 2, "")
		if err != nil {
			return nil
		}

		p := metricData{
			FetchResponse: pb.FetchResponse{
				Name:      proto.String(name),
				StartTime: proto.Int32(from),
				StopTime:  proto.Int32(until),
				StepTime:  proto.Int32(until - from),
				Values:    []float64{value, value},
				IsAbsent:  []bool{false, false},
			},
			color: color,
		}

		return []*metricData{&p}

	case "holtWintersForecast":
		var results []*metricData
		args, err := getSeriesArgs(e.args, from-7*86400, until, values)
		if err != nil {
			return nil
		}

		const alpha = 0.1
		const beta = 0.0035
		const gamma = 0.1

		for _, arg := range args {
			stepTime := arg.GetStepTime()
			numStepsToWalkToGetOriginalData := (int)((until - from) / stepTime)

			//originalSeries := arg.Values[len(arg.Values)-numStepsToWalkToGetOriginalData:]
			bootStrapSeries := arg.Values[:len(arg.Values)-numStepsToWalkToGetOriginalData]

			//In line with graphite, we define a season as a single day.
			//A period is the number of steps that make a season.
			period := (int)((24 * 60 * 60) / stepTime)

			predictions, err := holtwinters.Forecast(bootStrapSeries, alpha, beta, gamma, period, numStepsToWalkToGetOriginalData)
			if err != nil {
				return nil
			}

			predictionsOfInterest := predictions[len(predictions)-numStepsToWalkToGetOriginalData:]

			r := metricData{FetchResponse: pb.FetchResponse{
				Name:      proto.String(fmt.Sprintf("holtWintersForecast(%s)", arg.GetName())),
				Values:    make([]float64, len(predictionsOfInterest)),
				IsAbsent:  make([]bool, len(predictionsOfInterest)),
				StepTime:  proto.Int32(arg.GetStepTime()),
				StartTime: proto.Int32(arg.GetStartTime() + 7*86400),
				StopTime:  proto.Int32(arg.GetStopTime()),
			}}
			r.Values = predictionsOfInterest

			results = append(results, &r)
		}
		return results

	case "squareRoot": // squareRoot(seriesList)
		arg, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}
		var results []*metricData

		for _, a := range arg {
			r := *a
			r.Name = proto.String(fmt.Sprintf("squareRoot(%s)", a.GetName()))
			r.Values = make([]float64, len(a.Values))
			r.IsAbsent = make([]bool, len(a.Values))

			for i, v := range a.Values {
				if a.IsAbsent[i] {
					r.Values[i] = 0
					r.IsAbsent[i] = true
					continue
				}
				r.Values[i] = math.Sqrt(v)
			}
			results = append(results, &r)
		}
		return results

	case "randomWalk", "randomWalkFunction":
		name, err := getStringArg(e, 0)
		if err != nil {
			name = "randomWalk"
		}

		size := until - from

		r := metricData{FetchResponse: pb.FetchResponse{
			Name:      proto.String(name),
			Values:    make([]float64, size),
			IsAbsent:  make([]bool, size),
			StepTime:  proto.Int32(1),
			StartTime: proto.Int32(from),
			StopTime:  proto.Int32(until),
		}}

		for i := 1; i < len(r.Values)-1; i++ {
			r.Values[i+1] = r.Values[i] + (rand.Float64() - 0.5)
		}
		return []*metricData{&r}

	case "removeEmptySeries", "removeZeroSeries": // removeEmptySeries(seriesLists, n), removeZeroSeries(seriesLists, n)
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		var results []*metricData

		for _, a := range args {
			for i, v := range a.IsAbsent {
				if !v {
					if e.target == "removeEmptySeries" || (a.Values[i] != 0) {
						results = append(results, a)
						break
					}
				}
			}
		}
		return results

	case "removeBelowValue", "removeAboveValue", "removeBelowPercentile", "removeAbovePercentile": // removeBelowValue(seriesLists, n), removeAboveValue(seriesLists, n), removeBelowPercentile(seriesLists, percent), removeAbovePercentile(seriesLists, percent)
		args, err := getSeriesArg(e.args[0], from, until, values)
		if err != nil {
			return nil
		}

		number, err := getFloatArg(e, 1)
		if err != nil {
			return nil
		}

		condition := func(v float64, threshold float64) bool {
			return v < threshold
		}

		if strings.HasPrefix(e.target, "removeAbove") {
			condition = func(v float64, threshold float64) bool {
				return v > threshold
			}
		}

		var results []*metricData

		for _, a := range args {
			threshold := number
			if strings.HasSuffix(e.target, "Percentile") {
				var values []float64
				for i, v := range a.IsAbsent {
					if !v {
						values = append(values, a.Values[i])
					}
				}

				threshold = percentile(values, number, true)
			}

			r := *a
			r.Name = proto.String(fmt.Sprintf("%s(%s, %g)", e.target, a.GetName(), number))
			r.IsAbsent = make([]bool, len(a.Values))
			r.Values = make([]float64, len(a.Values))

			for i, v := range a.Values {
				if a.IsAbsent[i] || condition(v, threshold) {
					r.Values[i] = math.NaN()
					r.IsAbsent[i] = true
					continue
				}

				r.Values[i] = v
			}

			results = append(results, &r)
		}

		return results
	}

	logger.Logf("unknown function in evalExpr: %q\n", e.target)

	return nil
}

// Total (sortByTotal), max (sortByMaxima), min (sortByMinima) sorting
// For 'min', we actually store 1/v so the sorting logic is the same
type byVals struct {
	vals   []float64
	series []*metricData
}

func (s byVals) Len() int { return len(s.series) }
func (s byVals) Swap(i, j int) {
	s.series[i], s.series[j] = s.series[j], s.series[i]
	s.vals[i], s.vals[j] = s.vals[j], s.vals[i]
}
func (s byVals) Less(i, j int) bool {
	// actually "greater than"
	return s.vals[i] > s.vals[j]
}

// ByName sorts metrics by name
type ByName []*metricData

func (s ByName) Len() int           { return len(s) }
func (s ByName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s ByName) Less(i, j int) bool { return s[i].GetName() < s[j].GetName() }

// ByNameNatural sorts metric naturally by name
type ByNameNatural []*metricData

var dre = regexp.MustCompile(`\d+`)

func (s ByNameNatural) pad(str string) string {
	f := func(match []byte) []byte {
		n, _ := strconv.ParseInt(string(match), 10, 64)
		return []byte(fmt.Sprintf("%010d", n))
	}

	return string(dre.ReplaceAllFunc([]byte(str), f))
}
func (s ByNameNatural) Len() int           { return len(s) }
func (s ByNameNatural) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s ByNameNatural) Less(i, j int) bool { return s.pad(s[i].GetName()) < s.pad(s[j].GetName()) }

type seriesFunc func(*metricData, *metricData) *metricData

func forEachSeriesDo(e *expr, from, until int32, values map[metricRequest][]*metricData, function seriesFunc) []*metricData {
	arg, err := getSeriesArg(e.args[0], from, until, values)
	if err != nil {
		return nil
	}
	var results []*metricData

	for _, a := range arg {
		r := *a
		r.Name = proto.String(fmt.Sprintf("%s(%s)", e.target, a.GetName()))
		r.Values = make([]float64, len(a.Values))
		r.IsAbsent = make([]bool, len(a.Values))
		results = append(results, function(a, &r))
	}
	return results
}

type aggregateFunc func([]float64) float64

func aggregateSeries(e *expr, args []*metricData, function aggregateFunc) []*metricData {
	length := len(args[0].Values)
	r := *args[0]
	r.Name = proto.String(fmt.Sprintf("%s(%s)", e.target, e.argString))
	r.Values = make([]float64, length)
	r.IsAbsent = make([]bool, length)

	for i := range args[0].Values {
		var values []float64
		for _, arg := range args {
			if !arg.IsAbsent[i] {
				values = append(values, arg.Values[i])
			}
		}

		r.Values[i] = math.NaN()
		if len(values) > 0 {
			r.Values[i] = function(values)
		}

		r.IsAbsent[i] = math.IsNaN(r.Values[i])
	}

	return []*metricData{&r}
}

func summarizeValues(f string, values []float64) float64 {
	rv := 0.0

	if len(values) == 0 {
		return math.NaN()
	}

	switch f {
	case "sum":
		for _, av := range values {
			rv += av
		}

	case "avg":
		for _, av := range values {
			rv += av
		}
		rv /= float64(len(values))
	case "max":
		rv = math.Inf(-1)
		for _, av := range values {
			if av > rv {
				rv = av
			}
		}
	case "min":
		rv = math.Inf(1)
		for _, av := range values {
			if av < rv {
				rv = av
			}
		}
	case "last":
		if len(values) > 0 {
			rv = values[len(values)-1]
		}

	default:
		f = strings.Split(f, "p")[1]
		percent, err := strconv.ParseFloat(f, 64)
		if err == nil {
			rv = percentile(values, percent, true)
		}
	}

	return rv
}

func getBuckets(start, stop, bucketSize int32) int32 {
	return int32(math.Ceil(float64(stop-start) / float64(bucketSize)))
}

func alignStartToInterval(start, stop, bucketSize int32) int32 {
	for _, v := range []int32{86400, 3600, 60} {
		if bucketSize >= v {
			start -= start % v
			break
		}
	}

	return start
}

func alignToBucketSize(start, stop, bucketSize int32) (int32, int32) {
	start = int32(time.Unix(int64(start), 0).Truncate(time.Duration(bucketSize) * time.Second).Unix())
	newStop := int32(time.Unix(int64(stop), 0).Truncate(time.Duration(bucketSize) * time.Second).Unix())

	// check if a partial bucket is needed
	if stop != newStop {
		newStop += bucketSize
	}

	return start, newStop
}

func extractMetric(m string) string {

	// search for a metric name in `m'
	// metric name is defined to be a series of name characters terminated by a comma

	start := 0
	end := 0
	curlyBraces := 0
	for end < len(m) {
		if m[end] == '{' {
			curlyBraces++
		} else if m[end] == '}' {
			curlyBraces--
		} else if m[end] == ')' || (m[end] == ',' && curlyBraces == 0) {
			return m[start:end]
		} else if !(isNameChar(m[end]) || m[end] == ',') {
			start = end + 1
		}

		end++
	}

	return m[start:end]
}

func contains(a []int, i int) bool {
	for _, aa := range a {
		if aa == i {
			return true
		}
	}
	return false
}

// Based on github.com/dgryski/go-onlinestats
// Copied here because we don't need the rest of the package, and we only need
// a small part of this type which we need to modify anyway.

// Note that this uses a slightly unstable but faster implementation of
// standard deviation.  This is also required to be compatible with graphite.

type Windowed struct {
	data   []float64
	head   int
	length int
	sum    float64
	sumsq  float64
	nans   int
}

func (w *Windowed) Push(n float64) {
	old := w.data[w.head]

	w.length++

	w.data[w.head] = n
	w.head++
	if w.head >= len(w.data) {
		w.head = 0
	}

	if !math.IsNaN(old) {
		w.sum -= old
		w.sumsq -= (old * old)
	} else {
		w.nans--
	}

	if !math.IsNaN(n) {
		w.sum += n
		w.sumsq += (n * n)
	} else {
		w.nans++
	}
}

func (w *Windowed) Len() int {
	if w.length < len(w.data) {
		return w.length - w.nans
	}

	return len(w.data) - w.nans
}

func (w *Windowed) Stdev() float64 {
	l := w.Len()

	if l == 0 {
		return 0
	}

	n := float64(l)
	return math.Sqrt(n*w.sumsq-(w.sum*w.sum)) / n
}

func (w *Windowed) Mean() float64 { return w.sum / float64(w.Len()) }

func percentile(data []float64, percent float64, interpolate bool) float64 {
	if len(data) == 0 || percent < 0 || percent > 100 {
		return math.NaN()
	}
	if len(data) == 1 {
		return data[0]
	}

	k := (float64(len(data)-1) * percent) / 100
	length := int(math.Ceil(k)) + 1
	quickselect.Float64QuickSelect(data, length)
	top, secondTop := math.Inf(-1), math.Inf(-1)
	for _, val := range data[0:length] {
		if val > top {
			secondTop = top
			top = val
		} else if val > secondTop {
			secondTop = val
		}
	}
	remainder := k - float64(int(k))
	if remainder == 0 || !interpolate {
		return top
	}
	return (top * remainder) + (secondTop * (1 - remainder))
}

func maxValue(f64s []float64, absent []bool) float64 {
	m := math.Inf(-1)
	for i, v := range f64s {
		if absent[i] {
			continue
		}
		if v > m {
			m = v
		}
	}
	return m
}

func minValue(f64s []float64, absent []bool) float64 {
	m := math.Inf(1)
	for i, v := range f64s {
		if absent[i] {
			continue
		}
		if v < m {
			m = v
		}
	}
	return m
}

func avgValue(f64s []float64, absent []bool) float64 {
	var t float64
	var elts int
	for i, v := range f64s {
		if absent[i] {
			continue
		}
		elts++
		t += v
	}
	return t / float64(elts)
}

func currentValue(f64s []float64, absent []bool) float64 {

	for i := len(f64s) - 1; i >= 0; i-- {
		if !absent[i] {
			return f64s[i]
		}
	}

	return math.NaN()
}

func varianceValue(f64s []float64, absent []bool) float64 {
	var squareSum float64
	var elts int

	mean := avgValue(f64s, absent)
	if math.IsNaN(mean) {
		return mean
	}

	for i, v := range f64s {
		if absent[i] {
			continue
		}
		elts++
		squareSum += (mean - v) * (mean - v)
	}
	return squareSum / float64(elts)
}

type metricHeapElement struct {
	idx int
	val float64
}

type metricHeap []metricHeapElement

func (m metricHeap) Len() int           { return len(m) }
func (m metricHeap) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }
func (m metricHeap) Less(i, j int) bool { return m[i].val < m[j].val }

func (m *metricHeap) Push(x interface{}) {
	*m = append(*m, x.(metricHeapElement))
}

func (m *metricHeap) Pop() interface{} {
	old := *m
	n := len(old)
	x := old[n-1]
	*m = old[0 : n-1]
	return x
}
