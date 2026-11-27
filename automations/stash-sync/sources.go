package stashsync

import (
	"context"
	"errors"
)

// Source reads one number from a monitoring system. Each system is one Source, and reads only its own kind of
// Query.
type Source interface {
	Query(ctx context.Context, q Query) (float64, error)
}

// Query is one rule's query, such as a *CloudWatchQuery or a *PrometheusQuery.
type Query interface {
	// source is the name of the Source that runs the query.
	source() string
}

// ErrNoData is a query that ran and found no value. A rule with a default uses its default.
var ErrNoData = errors.New("no data")

func (r Rule) query() Query {
	switch {
	case r.CloudWatch != nil:
		return r.CloudWatch
	case r.Prometheus != nil:
		return r.Prometheus
	}
	return nil
}
