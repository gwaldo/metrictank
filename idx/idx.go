// The idx package provides a metadata index for metrics

package idx

import (
	"errors"
	"time"

	"github.com/raintank/met"
	"gopkg.in/raintank/schema.v1"
)

var (
	DefNotFound = errors.New("MetricDef not found")
)

type Node struct {
	Path string
	Leaf bool
	Defs []schema.MetricDefinition
}

/*
Currently the index is solely used for supporting Graphite style queries.
So, the index only needs to be able to search by a pattern that matches the
MetricDefinition.Name field. In future we plan to extend the searching
capabilities to include the other fields in the definition.

Note:

* metrictank is a multi-tenant system where different orgs cannot see each
  other's data

* any given metric may appear multiple times, under different organisations

* Each metric path can be mapped to multiple metricDefinitions in the case that
  fields other then the Name vary.  The most common occurrence of this is when
  the Interval at which the metric is being collected has changed.

Interface

* Init(met.Backend)
  This is the initialization step performed at startup. This method should
  block until the index is ready to handle searches.

* Stop():
 This will be called when metrictank is shutting down.

* Add(*schema.MetricData):
  Every metric received will result in a call to this method to ensure the
  metric has been added to the index.

* Get(string) (schema.MetricDefinition, error):
  This method should return the  MetricDefintion with the passed Id.

* List(int) []schema.MetricDefinition:
  This method should return all MetricDefinitions for the passed OrgId.  If the
  passed OrgId is "-1", then all metricDefinitions across all organisations
  should be returned.

* Find(int, string, int64) ([]Node, error):
  This method provides searches.  The method is passed an OrgId, a query
  pattern and a unix timestamp. Searches should return all nodes that match for
  the given OrgId and OrgId -1.  The pattern should be handled in the same way
  Graphite would. see https://graphite.readthedocs.io/en/latest/render_api.html#paths-and-wildcards
  And the unix stimestamp is used to ignore series that have been stale since
  the timestmap.

* Delete(int, string) ([]schema.MetricDefinition, error):
  This method is used for deleting items from the index. The method is passed
  an OrgId and a query pattern.  If the pattern matches a branch node, then
  all leaf nodes on that branch should also be deleted. So if the pattern is
  "*", all items in the index should be deleted.  A copy of all of the
  metricDefinitions deleted are returned.

* Prune(int, time.Time) ([]schema.MetricDefinition, error):
  This method should delete all metrics from the index for the passed org where
  the last time the metric was seen is older then the passed timestamp. If the org
  passed is -1, then the all orgs should be examined for stale metrics to be deleted.
  The method returns a list of the metricDefinitions deleted from the index and any
  error encountered.
*/
type MetricIndex interface {
	Init(met.Backend) error
	Stop()
	Add(*schema.MetricData)
	Get(string) (schema.MetricDefinition, error)
	Delete(int, string) ([]schema.MetricDefinition, error)
	Find(int, string, int64) ([]Node, error)
	List(int) []schema.MetricDefinition
	Prune(int, time.Time) ([]schema.MetricDefinition, error)
}
