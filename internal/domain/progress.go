package domain

// Progress is weighted completion. Done and Total are weight sums.
type Progress struct {
	Done     int
	Total    int
	Fraction float64 // 0..1
}

// Percent returns Fraction as 0..100.
func (p Progress) Percent() float64 { return p.Fraction * 100 }

// FeatureProgress computes a feature's progress: the weight of done tasks over
// the weight of non-cancelled tasks. A feature with no countable tasks is 0%.
func FeatureProgress(tasks []Task) Progress {
	var p Progress
	for _, t := range tasks {
		if t.Status == TaskCancelled {
			continue
		}
		p.Total += t.Weight
		if t.Status == TaskDone {
			p.Done += t.Weight
		}
	}
	if p.Total > 0 {
		p.Fraction = float64(p.Done) / float64(p.Total)
	}
	return p
}

// FeatureWithTasks pairs a feature with its tasks for project progress.
type FeatureWithTasks struct {
	Feature Feature
	Tasks   []Task
}

// FeatureEntry is one feature's progress inside a ProjectProgressResult.
type FeatureEntry struct {
	FeatureID int64
	Status    FeatureStatus // derived
	Progress  Progress
	Counted   bool // whether the feature contributed to the project figure
}

// ProjectProgressResult is the derived progress of a project.
type ProjectProgressResult struct {
	Progress Progress // Done/Total are feature-weight sums; Fraction is weighted
	Features []FeatureEntry
}

// ProjectProgress computes project progress:
//
//	sum(feature.weight * feature_progress) / sum(feature.weight)
//
// over counted features. Cancelled features are excluded, and so are features
// whose tasks are all cancelled (that work no longer counts). A feature with
// no tasks at all is counted at 0%, because planned work that has not been
// broken down must hold the figure back. Done is the summed weight of
// features that are fully done. A project with no counted features is 0%.
func ProjectProgress(features []FeatureWithTasks) ProjectProgressResult {
	var res ProjectProgressResult
	var weighted float64
	for _, f := range features {
		entry := FeatureEntry{
			FeatureID: f.Feature.ID,
			Status:    DeriveFeatureStatus(f.Feature.Status, f.Tasks),
			Progress:  FeatureProgress(f.Tasks),
		}
		allCancelled := len(f.Tasks) > 0 && entry.Progress.Total == 0
		entry.Counted = entry.Status != FeatureCancelled && !allCancelled
		if entry.Counted {
			res.Progress.Total += f.Feature.Weight
			weighted += float64(f.Feature.Weight) * entry.Progress.Fraction
			if entry.Status == FeatureDone {
				res.Progress.Done += f.Feature.Weight
			}
		}
		res.Features = append(res.Features, entry)
	}
	if res.Progress.Total > 0 {
		res.Progress.Fraction = weighted / float64(res.Progress.Total)
	}
	return res
}
