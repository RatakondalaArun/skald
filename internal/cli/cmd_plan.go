package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"skald/internal/domain"
	"skald/internal/service"
)

const planHelp = `Creates features, tasks and dependencies from a JSON file in one transaction.
Nothing is written if any part is invalid. Use "-" to read standard input.

File format (docs/plan-example.json has a complete example):

  {
    "features": [
      {
        "title": "Authentication",        required
        "description": "Sign in and out",
        "weight": 3,                      1-10, default 1
        "tasks": [
          {
            "ref": "schema",              required; names the task inside this file
            "title": "User table",        required
            "description": "...",
            "acceptance_criteria": "...",
            "priority": 1,                0 (urgent) to 3 (low), default 2
            "weight": 1,                  1-10, default 1
            "depends_on": ["other-ref"],  refs of tasks in this file
            "depends_on_existing": ["T-3"], tasks already in the project
            "idempotency_key": "auth-schema"
          }
        ]
      }
    ]
  }

Re-running a plan is safe when every task has an idempotency_key: tasks that
already exist are reused, and a feature is reused (matched by title) when all of
its tasks have keys. Tasks without a key are created again on every run.`

type planFile struct {
	Features []planFeatureFile `json:"features"`
}

type planFeatureFile struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Weight      int            `json:"weight"`
	Tasks       []planTaskFile `json:"tasks"`
}

type planTaskFile struct {
	Ref                string       `json:"ref"`
	Title              string       `json:"title"`
	Description        string       `json:"description"`
	AcceptanceCriteria string       `json:"acceptance_criteria"`
	Priority           *int         `json:"priority"`
	Weight             int          `json:"weight"`
	DependsOn          []string     `json:"depends_on"`
	DependsOnExisting  []existingID `json:"depends_on_existing"`
	IdempotencyKey     string       `json:"idempotency_key"`
}

// existingID accepts a task ID as "T-3", "3" or the number 3.
type existingID int64

func (x *existingID) UnmarshalJSON(b []byte) error {
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*x = existingID(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("depends_on_existing entries must be task IDs such as \"T-3\"")
	}
	id, err := domain.ParseID(domain.EntityTask, s)
	if err != nil {
		return err
	}
	*x = existingID(id)
	return nil
}

func parsePlan(data []byte) (service.PlanInput, error) {
	var f planFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return service.PlanInput{}, &usageError{msg: "invalid plan file: " + err.Error()}
	}
	if dec.More() {
		return service.PlanInput{}, usagef("invalid plan file: unexpected data after the JSON object")
	}
	var in service.PlanInput
	for _, pf := range f.Features {
		feat := service.PlanFeature{Title: pf.Title, Description: pf.Description, Weight: pf.Weight}
		for _, pt := range pf.Tasks {
			t := service.PlanTask{
				Ref: pt.Ref, Title: pt.Title, Description: pt.Description, AcceptanceCriteria: pt.AcceptanceCriteria,
				Priority: pt.Priority, Weight: pt.Weight, DependsOn: pt.DependsOn, IdempotencyKey: pt.IdempotencyKey,
			}
			for _, id := range pt.DependsOnExisting {
				t.DependsOnExisting = append(t.DependsOnExisting, int64(id))
			}
			feat.Tasks = append(feat.Tasks, t)
		}
		in.Features = append(in.Features, feat)
	}
	return in, nil
}

func newPlanCmd(a *app) *cobra.Command {
	imp := &cobra.Command{
		Use:     "import <file.json|->",
		Short:   "Create features, tasks and dependencies from a JSON plan",
		Long:    planHelp,
		Example: "  skald plan import plan.json\n  cat plan.json | skald plan import -",
		Args:    exactArgs(1),
		RunE: a.do(func(e *env, c *cobra.Command, args []string) error {
			var data []byte
			var err error
			if args[0] == "-" {
				data, err = io.ReadAll(c.InOrStdin())
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil {
				return err
			}
			in, err := parsePlan(data)
			if err != nil {
				return err
			}
			res, err := e.svc.CreatePlan(e.ctx, e.actor, e.ref, in)
			if err != nil {
				return err
			}
			return e.emit(toPlanResult(res), func(w io.Writer) {
				fmt.Fprintf(w, "plan imported into %s: %d features (%d new), %d tasks (%d new), %d dependencies added\n",
					res.ProjectSlug, len(res.Features), res.FeaturesCreated, len(res.Tasks), res.TasksCreated, res.DependenciesAdded)
				refs := make([]string, 0, len(res.Tasks))
				for r := range res.Tasks {
					refs = append(refs, r)
				}
				sort.Slice(refs, func(i, j int) bool { return res.Tasks[refs[i]].TaskID < res.Tasks[refs[j]].TaskID })
				t := table(w)
				fmt.Fprintln(t, "REF\tTASK\tFEATURE\t")
				for _, r := range refs {
					p := res.Tasks[r]
					state := "new"
					if !p.Created {
						state = "existing"
					}
					fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", r, taskID(p.TaskID), featureID(p.FeatureID), state)
				}
				t.Flush()
			})
		}),
	}
	return group("plan", "Import whole plans", imp)
}

type planResultJSON struct {
	Project           string                  `json:"project"`
	Features          []planFeatureJSON       `json:"features"`
	Tasks             map[string]planTaskJSON `json:"tasks"`
	FeaturesCreated   int                     `json:"features_created"`
	FeaturesReused    int                     `json:"features_reused"`
	TasksCreated      int                     `json:"tasks_created"`
	TasksReused       int                     `json:"tasks_reused"`
	DependenciesAdded int                     `json:"dependencies_added"`
}

type planFeatureJSON struct {
	Title   string `json:"title"`
	ID      string `json:"id"`
	Created bool   `json:"created"`
}

type planTaskJSON struct {
	ID      string `json:"id"`
	Feature string `json:"feature"`
	Created bool   `json:"created"`
}

func toPlanResult(r service.PlanResult) planResultJSON {
	j := planResultJSON{
		Project: r.ProjectSlug, Features: []planFeatureJSON{}, Tasks: map[string]planTaskJSON{},
		FeaturesCreated: r.FeaturesCreated, FeaturesReused: r.FeaturesReused,
		TasksCreated: r.TasksCreated, TasksReused: r.TasksReused, DependenciesAdded: r.DependenciesAdded,
	}
	for _, f := range r.Features {
		j.Features = append(j.Features, planFeatureJSON{f.Title, featureID(f.FeatureID), f.Created})
	}
	for ref, t := range r.Tasks {
		j.Tasks[ref] = planTaskJSON{taskID(t.TaskID), featureID(t.FeatureID), t.Created}
	}
	return j
}
