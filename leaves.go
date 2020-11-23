package leaves

import (
	"fmt"
	"math"
	"runtime"
	"sync"

	"github.com/fredrikluo/leaves/transformation"
)

// BatchSize for parallel task
const BatchSize = 16

type ensembleBaseInterface interface {
	NLeaves() []int
	NEstimators() int
	NRawOutputGroups() int
	NFeatures() int
	Name() string
	adjustNEstimators(nEstimators int) int
	predictInner(fvals []float64, nEstimators int, predictions []float64, startIndex int, predictionLeafIndices [][]uint32)
	resetFVals(fvals []float64)
}

// Ensemble is a common wrapper for all models
type Ensemble struct {
	ensembleBaseInterface
	transform transformation.Transform
}

func (e *Ensemble) predictInnerAndTransform(fvals []float64, nEstimators int, predictions []float64, startIndex int, predictionLeafIndices [][]uint32) {
	if e.Transformation().Type() == transformation.Raw {
		e.predictInner(fvals, nEstimators, predictions, startIndex, predictionLeafIndices)
	} else {
		// TODO: avoid allocation here
		rawPredictions := make([]float64, e.NRawOutputGroups())
		e.predictInner(fvals, nEstimators, rawPredictions, 0, predictionLeafIndices)
		e.transform.Transform(rawPredictions, predictions, startIndex)
	}
}

// PredictSingle calculates prediction for single class model. If ensemble is
// multiclass, will return quitely 0.0. Only `nEstimators` first estimators
// (trees in most cases) will be used. If `len(fvals)` is not enough function
// will quietly return 0.0.
// If `predleaf` is set to true, it will also return leaf index which predicts the
// result from each estimator.
// NOTE: for multiclass prediction use Predict
func (e *Ensemble) PredictSingle(fvals []float64, nEstimators int, predleaf bool) (float64, []uint32) {
	if e.NOutputGroups() != 1 {
		return 0.0, nil
	}
	if e.NFeatures() > len(fvals) {
		return 0.0, nil
	}
	nEstimators = e.adjustNEstimators(nEstimators)
	ret := [1]float64{0.0}

	if predleaf {
		predictionLeafIndicesResult := e.allocPredictionLeafIndices(1)
		e.predictInnerAndTransform(fvals, nEstimators, ret[:], 0, predictionLeafIndicesResult)
		return ret[0], predictionLeafIndicesResult[0]
	}

	e.predictInnerAndTransform(fvals, nEstimators, ret[:], 0, nil)
	return ret[0], nil
}

// Predict calculates single prediction for one or multiclass ensembles. Only
// `nEstimators` first estimators (trees in most cases) will be used.
// If `predleaf` is set to true, it will also return leaf index which predicts the
// result from each estimator.
// NOTE: for single class predictions one can use simplified function PredictSingle
func (e *Ensemble) Predict(fvals []float64, nEstimators int, predictions []float64, predleaf bool) ([][]uint32, error) {
	nRows := 1
	if len(predictions) < e.NOutputGroups()*nRows {
		return nil, fmt.Errorf("predictions slice too short (should be at least %d)", e.NOutputGroups()*nRows)
	}
	if e.NFeatures() > len(fvals) {
		return nil, fmt.Errorf("incorrect number of features (%d)", len(fvals))
	}
	nEstimators = e.adjustNEstimators(nEstimators)

	if predleaf {
		predictionLeafIndicesResult := e.allocPredictionLeafIndices(len(predictions))
		e.predictInnerAndTransform(fvals, nEstimators, predictions, 0, predictionLeafIndicesResult)
		return predictionLeafIndicesResult, nil
	}

	e.predictInnerAndTransform(fvals, nEstimators, predictions, 0, nil)
	return nil, nil
}

func (e *Ensemble) allocPredictionLeafIndices(dim int) [][]uint32 {
	predictionLeafIndicesResult := make([][]uint32, dim)
	for i := 0; i < dim; i++ {
		predictionLeafIndicesResult[i] = make([]uint32, e.NEstimators())
	}
	return predictionLeafIndicesResult
}

// PredictCSR calculates predictions from ensemble. `indptr`, `cols`, `vals`
// represent data structures from Compressed Sparse Row Matrix format (see
// CSRMat). Only `nEstimators` first estimators (trees) will be used.
// `nThreads` points to number of threads that will be utilized (maximum
// is GO_MAX_PROCS)
// If `predleaf` is set to true, it will also return leaf index which predicts the
// result from each estimator.
// Note, `predictions` slice should be properly allocated on call side
func (e *Ensemble) PredictCSR(indptr []int, cols []int, vals []float64, predictions []float64, nEstimators int, nThreads int, predleaf bool) ([][]uint32, error) {
	nRows := len(indptr) - 1
	if len(predictions) < e.NOutputGroups()*nRows {
		return nil, fmt.Errorf("predictions slice too short (should be at least %d)", e.NOutputGroups()*nRows)
	}
	nEstimators = e.adjustNEstimators(nEstimators)
	var predictionLeafIndicesResult [][]uint32
	if predleaf {
		predictionLeafIndicesResult = e.allocPredictionLeafIndices(len(predictions))
	}

	if nRows <= BatchSize || nThreads == 0 || nThreads == 1 {
		// single thread calculations
		fvals := make([]float64, e.NFeatures())
		e.resetFVals(fvals)
		e.predictCSRInner(indptr, cols, vals, 0, len(indptr)-1, predictions, nEstimators, fvals, predictionLeafIndicesResult)
		return predictionLeafIndicesResult, nil
	}
	if nThreads > runtime.GOMAXPROCS(0) || nThreads < 1 {
		nThreads = runtime.GOMAXPROCS(0)
	}
	nBatches := int(math.Ceil(float64(nRows) / BatchSize))
	if nThreads > nBatches {
		nThreads = nBatches
	}
	tasks := make(chan int)

	wg := sync.WaitGroup{}
	for i := 0; i < nThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fvals := make([]float64, e.NFeatures())
			e.resetFVals(fvals)
			for startIndex := range tasks {
				endIndex := startIndex + BatchSize
				if endIndex > nRows {
					endIndex = nRows
				}

				e.predictCSRInner(indptr, cols, vals, startIndex, endIndex, predictions, nEstimators, fvals, predictionLeafIndicesResult)
			}
		}()
	}

	// feed the queue
	for i := 0; i < nBatches; i++ {
		tasks <- i * BatchSize
	}
	close(tasks)
	wg.Wait()
	return predictionLeafIndicesResult, nil
}

func (e *Ensemble) predictCSRInner(
	indptr []int,
	cols []int,
	vals []float64,
	startIndex int,
	endIndex int,
	predictions []float64,
	nEstimators int,
	fvals []float64,
	predictionLeafIndices [][]uint32,
) {
	for i := startIndex; i < endIndex; i++ {
		start := indptr[i]
		end := indptr[i+1]
		for j := start; j < end; j++ {
			if cols[j] < len(fvals) {
				fvals[cols[j]] = vals[j]
			}
		}

		e.predictInnerAndTransform(fvals, nEstimators, predictions, i*e.NOutputGroups(), predictionLeafIndices)
		e.resetFVals(fvals)
	}
}

// PredictDense calculates predictions from ensemble. `vals`, `rows`, `cols`
// represent data structures from Rom Major Matrix format (see DenseMat). Only
// `nEstimators` first estimators (trees in most cases) will be used. `nThreads`
// points to number of threads that will be utilized (maximum is GO_MAX_PROCS).
// If `predleaf` is set to true, it will also return leaf index which predicts the
// result from each estimator.
// Note, `predictions` slice should be properly allocated on call side
func (e *Ensemble) PredictDense(
	vals []float64,
	nrows int,
	ncols int,
	predictions []float64,
	nEstimators int,
	nThreads int,
	predleaf bool,
) ([][]uint32, error) {
	nRows := nrows
	if len(predictions) < e.NOutputGroups()*nRows {
		return nil, fmt.Errorf("predictions slice too short (should be at least %d)", e.NOutputGroups()*nRows)
	}
	if ncols == 0 || e.NFeatures() > ncols {
		return nil, fmt.Errorf("incorrect number of columns")
	}
	nEstimators = e.adjustNEstimators(nEstimators)
	var predictionLeafIndicesResult [][]uint32
	if predleaf {
		predictionLeafIndicesResult = e.allocPredictionLeafIndices(len(predictions))
	}
	if nRows <= BatchSize || nThreads == 0 || nThreads == 1 {
		// single thread calculations
		for i := 0; i < nRows; i++ {
			fvals := vals[i*ncols : (i+1)*ncols]
			e.predictInnerAndTransform(fvals, nEstimators, predictions, i*e.NOutputGroups(), predictionLeafIndicesResult)
		}
		return predictionLeafIndicesResult, nil
	}
	if nThreads > runtime.GOMAXPROCS(0) || nThreads < 1 {
		nThreads = runtime.GOMAXPROCS(0)
	}
	nBatches := int(math.Ceil(float64(nRows) / BatchSize))
	if nThreads > nBatches {
		nThreads = nBatches
	}
	tasks := make(chan int)

	wg := sync.WaitGroup{}
	for i := 0; i < nThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for startIndex := range tasks {
				endIndex := startIndex + BatchSize
				if endIndex > nRows {
					endIndex = nRows
				}
				for i := startIndex; i < endIndex; i++ {
					e.predictInnerAndTransform(vals[i*int(ncols):(i+1)*int(ncols)], nEstimators, predictions, i*e.NOutputGroups(), predictionLeafIndicesResult)
				}
			}
		}()
	}

	// feed the queue
	for i := 0; i < nBatches; i++ {
		tasks <- i * BatchSize
	}
	close(tasks)
	wg.Wait()
	return predictionLeafIndicesResult, nil
}

// NEstimators returns number of estimators (trees) in ensemble (per group)
func (e *Ensemble) NEstimators() int {
	return e.ensembleBaseInterface.NEstimators()
}

// NRawOutputGroups returns number of groups (numbers) in every object
// predictions before transformation function applied. This value is provided
// mainly for information purpose
func (e *Ensemble) NRawOutputGroups() int {
	return e.ensembleBaseInterface.NRawOutputGroups()
}

// NOutputGroups returns number of groups (numbers) in every object predictions.
// For example binary logistic model will give 1, but 4-class prediction model
// will give 4 numbers per object. This value usually used to preallocate slice
// for prediction values
func (e *Ensemble) NOutputGroups() int {
	return e.transform.NOutputGroups()
}

// NFeatures returns number of features in the model
func (e *Ensemble) NFeatures() int {
	return e.ensembleBaseInterface.NFeatures()
}

// Name returns name of the estimator
func (e *Ensemble) Name() string {
	return e.ensembleBaseInterface.Name()
}

// Transformation returns transformation objects which applied to model outputs.
func (e *Ensemble) Transformation() transformation.Transform {
	return e.transform
}

// EnsembleWithRawPredictions returns ensemble instance with TransformRaw (no
// transformation functions will be applied to the model resulst)
func (e *Ensemble) EnsembleWithRawPredictions() *Ensemble {
	return &Ensemble{e, &transformation.TransformRaw{e.NRawOutputGroups()}}
}
