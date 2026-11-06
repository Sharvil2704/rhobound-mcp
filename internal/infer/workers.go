// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"fmt"
	"sort"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/sanitize"
)

// ShownWorkers returns workers with each service named as the traces wrote it: a name the views show for one of the
// capture's services (sanitize.Services: an ARN's last element, svc_<hash8>) becomes that service's, so that a caller
// can pin the workers of a service whose name the views replace (fit --workers, and the MCP server's fit_start). A
// name the views show for more than one service is refused with change.ambiguous_service; any other is left as given,
// for Run to judge. Report v1's path (predict CAPTURE --workers) does not call it: its names are the traces', as
// rhobound-cli read them.
func ShownWorkers(d *otlp.Data, workers map[string]int) (map[string]int, error) {
	if len(workers) == 0 {
		return workers, nil
	}
	var names []string
	seen := map[string]bool{}
	for _, r := range d.Resources {
		if !seen[r.Service] {
			seen[r.Service] = true
			names = append(names, r.Service)
		}
	}
	shown := sanitize.Services(names)
	given := make([]string, 0, len(workers))
	for n := range workers {
		given = append(given, n)
	}
	sort.Strings(given)
	out := make(map[string]int, len(workers))
	for _, n := range given {
		node, ok, ambiguous := sanitize.Resolve(shown, n)
		if ambiguous {
			return nil, &Refusal{Code: contract.ChangeAmbiguousService, Subject: n,
				Err: fmt.Errorf("--workers: the views show more than one service as %q", n)}
		}
		if !ok {
			node = n
		}
		out[node] = workers[n]
	}
	return out, nil
}
