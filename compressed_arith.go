package sparse

import (
	"github.com/james-bowman/sparse/blas"
	"gonum.org/v1/gonum/mat"
)

// MulMatRawVec computes the matrix vector product between lhs and rhs and stores
// the result in out
func MulMatRawVec(lhs *CSR, rhs []float64, out []float64) {
	m, n := lhs.Dims()
	if len(rhs) != n {
		panic(mat.ErrShape)
	}
	if len(out) != m {
		panic(mat.ErrShape)
	}

	blas.Dusmv(false, 1, lhs.RawMatrix(), rhs, 1, out, 1)
}

// temporaryWorkspace returns a new CSR matrix w with the size of r x c with
// initial capacity allocated for nnz non-zero elements and
// returns a callback to defer which performs cleanup at the return of the call.
// This should be used when a method receiver is the same pointer as an input argument.
func (c *CSR) temporaryWorkspace(row, col, nnz int, clear bool) (w *CSR, restore func()) {
	w = getWorkspace(row, col, nnz, clear)
	return w, func() {
		c.cloneCSR(w)
		putWorkspace(w)
	}
}

// spalloc ensures appropriate storage is allocated for the receiver sparse matrix
// ensuring it is row * col dimensions and checking for any overlap or aliasing
// between operands a or b with c in which case a temporary isolated workspace is
// allocated and the returned value isTemp is true with restore representing a
// function to clean up and restore the workspace once finished.
func (c *CSR) spalloc(a mat.Matrix, b mat.Matrix) (m *CSR, isTemp bool, restore func()) {
	var nnz int
	m = c
	row, _ := a.Dims()
	_, col := b.Dims()

	lSp, lIsSp := a.(Sparser)
	rSp, rIsSp := b.(Sparser)
	if lIsSp && rIsSp {
		nnz = lSp.NNZ() + rSp.NNZ()
	} else {
		// assume 10% of elements will be non-zero
		nnz = row * col / 10
	}

	if c.checkOverlap(a) || c.checkOverlap(b) {
		if !c.IsZero() && (row != c.matrix.I || col != c.matrix.J) {
			panic(mat.ErrShape)
		}
		m, restore = c.temporaryWorkspace(row, col, nnz, true)
		isTemp = true
	} else {
		c.reuseAs(row, col, nnz, true)
	}

	return
}

// Mul takes the matrix product of the supplied matrices a and b and stores the result
// in the receiver.  Some specific optimisations are available for operands of certain
// sparse formats e.g. CSR * CSR uses Gustavson Algorithm (ACM 1978) for fast
// sparse matrix multiplication.
// If the number of columns does not equal the number of rows in b, Mul will panic.
func (c *CSR) Mul(a, b mat.Matrix) {
	ar, ac := a.Dims()
	br, bc := b.Dims()

	if ac != br {
		panic(mat.ErrShape)
	}

	if m, temp, restore := c.spalloc(a, b); temp {
		defer restore()
		c = m
	}

	lhs, isLCsr := a.(*CSR)
	rhs, isRCsr := b.(*CSR)
	if isLCsr && isRCsr {
		// handle CSR * CSR
		c.mulCSRCSR(lhs, rhs)
		return
	}

	if dia, ok := a.(*DIA); ok {
		if isRCsr {
			// handle DIA * CSR
			c.mulDIACSR(dia, rhs, false)
			return
		}
		// handle DIA * mat.Matrix
		c.mulDIAMat(dia, b, false)
		return
	}
	if dia, ok := b.(*DIA); ok {
		if isLCsr {
			// handle CSR * DIA
			c.mulDIACSR(dia, lhs, true)
			return
		}
		// handle mat.Matrix * DIA
		c.mulDIAMat(dia, a, true)
		return
	}
	// TODO: handle cases where both matrices are DIA

	srcA, isLSparse := a.(TypeConverter)
	srcB, isRSparse := b.(TypeConverter)
	if isLSparse {
		if isRSparse {
			// handle Sparser * Sparser
			c.mulCSRCSR(srcA.ToCSR(), srcB.ToCSR())
			return
		}
		// handle Sparser * mat.Matrix
		c.mulCSRMat(srcA.ToCSR(), b)
		return
	}
	if isRSparse {
		// handle mat.Matrix * Sparser
		w := getWorkspace(bc, ar, bc*ar/10, true)
		bt := srcB.ToCSC().T().(*CSR)
		w.mulCSRMat(bt, a.T())
		c.Clone(w.T())
		putWorkspace(w)
		return
	}

	// handle mat.Matrix * mat.Matrix
	row := getFloats(ac, false)
	defer putFloats(row)
	var v float64
	for i := 0; i < ar; i++ {
		for ci := range row {
			row[ci] = a.At(i, ci)
		}
		for j := 0; j < bc; j++ {
			v = 0
			for ci, e := range row {
				if e != 0 {
					v += e * b.At(ci, j)
				}
			}
			if v != 0 {
				c.matrix.Ind = append(c.matrix.Ind, j)
				c.matrix.Data = append(c.matrix.Data, v)
			}
		}
		c.matrix.Indptr[i+1] = len(c.matrix.Ind)
	}
}

// mulCSRCSR handles CSR = CSR * CSR using Gustavson Algorithm (ACM 1978)
func (c *CSR) mulCSRCSR(lhs *CSR, rhs *CSR) {
	ar, _ := lhs.Dims()
	_, bc := rhs.Dims()
	spa := NewSPA(bc)

	// rows in C
	for i := 0; i < ar; i++ {
		// each element t in row i of A
		for t := lhs.matrix.Indptr[i]; t < lhs.matrix.Indptr[i+1]; t++ {
			begin := rhs.matrix.Indptr[lhs.matrix.Ind[t]]
			end := rhs.matrix.Indptr[lhs.matrix.Ind[t]+1]
			spa.Scatter(rhs.matrix.Data[begin:end], rhs.matrix.Ind[begin:end], lhs.matrix.Data[t], &c.matrix.Ind)
		}
		spa.GatherAndZero(&c.matrix.Data, &c.matrix.Ind)
		c.matrix.Indptr[i+1] = len(c.matrix.Ind)
	}
}

// mulCSRMat handles CSR = CSR * mat.Matrix
func (c *CSR) mulCSRMat(lhs *CSR, b mat.Matrix) {
	ar, _ := lhs.Dims()
	_, bc := b.Dims()

	// handle case where matrix A is CSR (matrix B can be any implementation of mat.Matrix)
	for i := 0; i < ar; i++ {
		for j := 0; j < bc; j++ {
			var v float64
			// TODO Consider converting all Sparser args to CSR
			for k := lhs.matrix.Indptr[i]; k < lhs.matrix.Indptr[i+1]; k++ {
				v += lhs.matrix.Data[k] * b.At(lhs.matrix.Ind[k], j)
			}
			if v != 0 {
				c.matrix.Ind = append(c.matrix.Ind, j)
				c.matrix.Data = append(c.matrix.Data, v)
			}
		}
		c.matrix.Indptr[i+1] = len(c.matrix.Ind)
	}
}

// mulDIACSR handles CSR = DIA * CSR (or CSR = CSR * DIA if trans == true)
func (c *CSR) mulDIACSR(dia *DIA, other *CSR, trans bool) {
	diagonal := dia.Diagonal()
	if trans {
		for i := 0; i < c.matrix.I; i++ {
			var v float64
			for k := other.matrix.Indptr[i]; k < other.matrix.Indptr[i+1]; k++ {
				if other.matrix.Ind[k] < len(diagonal) {
					v = other.matrix.Data[k] * diagonal[other.matrix.Ind[k]]
					if v != 0 {
						c.matrix.Ind = append(c.matrix.Ind, other.matrix.Ind[k])
						c.matrix.Data = append(c.matrix.Data, v)
					}
				}
			}
			c.matrix.Indptr[i+1] = len(c.matrix.Ind)
		}
	} else {
		for i := 0; i < c.matrix.I; i++ {
			var v float64
			for k := other.matrix.Indptr[i]; k < other.matrix.Indptr[i+1]; k++ {
				if i < len(diagonal) {
					v = other.matrix.Data[k] * diagonal[i]
					if v != 0 {
						c.matrix.Ind = append(c.matrix.Ind, other.matrix.Ind[k])
						c.matrix.Data = append(c.matrix.Data, v)
					}
				}
			}
			c.matrix.Indptr[i+1] = len(c.matrix.Ind)
		}
	}
}

// mulDIAMat handles CSR = DIA * mat.Matrix (or CSR = mat.Matrix * DIA if trans == true)
func (c *CSR) mulDIAMat(dia *DIA, other mat.Matrix, trans bool) {
	_, cols := other.Dims()
	diagonal := dia.Diagonal()

	if trans {
		for i := 0; i < c.matrix.I; i++ {
			var v float64
			for k := 0; k < cols; k++ {
				if k < len(diagonal) {
					v = other.At(i, k) * diagonal[k]
					if v != 0 {
						c.matrix.Ind = append(c.matrix.Ind, k)
						c.matrix.Data = append(c.matrix.Data, v)
					}
				}
			}
			c.matrix.Indptr[i+1] = len(c.matrix.Ind)
		}
	} else {
		for i := 0; i < c.matrix.I; i++ {
			var v float64
			for k := 0; k < cols; k++ {
				if i < len(diagonal) {
					v = other.At(i, k) * diagonal[i]
					if v != 0 {
						c.matrix.Ind = append(c.matrix.Ind, k)
						c.matrix.Data = append(c.matrix.Data, v)
					}
				}
			}
			c.matrix.Indptr[i+1] = len(c.matrix.Ind)
		}
	}
}

// Sub subtracts matrix b from a and stores the result in the receiver.
// If matrices a and b are not the same shape then the method will panic.
func (c *CSR) Sub(a, b mat.Matrix) {
	c.addScaled(a, b, 1, -1)
}

// Add adds matrices a and b together and stores the result in the receiver.
// If matrices a and b are not the same shape then the method will panic.
func (c *CSR) Add(a, b mat.Matrix) {
	c.addScaled(a, b, 1, 1)
}

// addScaled adds matrices a and b scaling them by a and b respectively before hand.
func (c *CSR) addScaled(a mat.Matrix, b mat.Matrix, alpha float64, beta float64) {
	ar, ac := a.Dims()
	br, bc := b.Dims()

	if ar != br || ac != bc {
		panic(mat.ErrShape)
	}

	if m, temp, restore := c.spalloc(a, b); temp {
		defer restore()
		c = m
	}

	lCsr, lIsCsr := a.(*CSR)
	rCsr, rIsCsr := b.(*CSR)
	// TODO optimisation for DIA matrices
	if lIsCsr && rIsCsr {
		c.addCSRCSR(lCsr, rCsr, alpha, beta)
		return
	}
	if lIsCsr {
		c.addCSR(lCsr, b, alpha, beta)
		return
	}
	if rIsCsr {
		c.addCSR(rCsr, a, beta, alpha)
		return
	}
	// dumb addition with no sparcity optimisations/savings
	for i := 0; i < ar; i++ {
		for j := 0; j < ac; j++ {
			v := alpha*a.At(i, j) + beta*b.At(i, j)
			if v != 0 {
				c.matrix.Ind = append(c.matrix.Ind, j)
				c.matrix.Data = append(c.matrix.Data, v)
			}
		}
		c.matrix.Indptr[i+1] = len(c.matrix.Ind)
	}
}

// addCSR adds a CSR matrix to any implementation of mat.Matrix and stores the
// result in the receiver.
func (c *CSR) addCSR(csr *CSR, other mat.Matrix, alpha float64, beta float64) {
	ar, ac := csr.Dims()
	spa := NewSPA(ac)
	a := csr.RawMatrix()

	if dense, isDense := other.(mat.RawMatrixer); isDense {
		for i := 0; i < ar; i++ {
			begin := csr.matrix.Indptr[i]
			end := csr.matrix.Indptr[i+1]
			rawOther := dense.RawMatrix()
			r := rawOther.Data[i*rawOther.Stride : i*rawOther.Stride+rawOther.Cols]
			spa.AccumulateDense(r, beta, &c.matrix.Ind)
			spa.Scatter(a.Data[begin:end], a.Ind[begin:end], alpha, &c.matrix.Ind)
			spa.GatherAndZero(&c.matrix.Data, &c.matrix.Ind)
			c.matrix.Indptr[i+1] = len(c.matrix.Ind)
		}
	} else {
		for i := 0; i < ar; i++ {
			begin := csr.matrix.Indptr[i]
			end := csr.matrix.Indptr[i+1]
			for j := 0; j < ac; j++ {
				v := other.At(i, j)
				if v != 0 {
					spa.ScatterValue(v, j, beta, &c.matrix.Ind)
				}
			}
			spa.Scatter(a.Data[begin:end], a.Ind[begin:end], alpha, &c.matrix.Ind)
			spa.GatherAndZero(&c.matrix.Data, &c.matrix.Ind)
			c.matrix.Indptr[i+1] = len(c.matrix.Ind)
		}
	}
}

// addCSRCSR adds 2 CSR matrices together storing the result in the receiver.
// Matrices a and b are scaled by alpha and beta respectively before addition.
// This method is specially optimised to take advantage of the sparsity patterns
// of the 2 CSR matrices.
func (c *CSR) addCSRCSR(lhs *CSR, rhs *CSR, alpha float64, beta float64) {
	ar, ac := lhs.Dims()
	a := lhs.RawMatrix()
	b := rhs.RawMatrix()
	spa := NewSPA(ac)

	var begin, end int
	for i := 0; i < ar; i++ {
		begin, end = a.Indptr[i], a.Indptr[i+1]
		spa.Scatter(a.Data[begin:end], a.Ind[begin:end], alpha, &c.matrix.Ind)

		begin, end = b.Indptr[i], b.Indptr[i+1]
		spa.Scatter(b.Data[begin:end], b.Ind[begin:end], beta, &c.matrix.Ind)

		spa.GatherAndZero(&c.matrix.Data, &c.matrix.Ind)
		c.matrix.Indptr[i+1] = len(c.matrix.Ind)
	}
}

// SPA is a SParse Accumulator used to construct the results of sparse
// arithmetic operations in linear time.
type SPA struct {
	// w contains flags for indices containing non-zero values
	w []int

	// x contains all the values in dense representation (including zero values)
	y []float64

	// nnz is the Number of Non-Zero elements
	nnz int

	// generation is used to compare values of w to see if they have been set
	// in the current row (generation).  This avoids needing to reset all values
	// during the GatherAndZero operation at the end of
	// construction for each row/column vector.
	generation int
}

// NewSPA creates a new SParse Accumulator of length n.  If accumulating
// rows for a CSR matrix then n should be equal to the number of columns
// in the resulting matrix.
func NewSPA(n int) *SPA {
	return &SPA{
		w: make([]int, n),
		y: make([]float64, n),
	}
}

// ScatterVec accumulates the sparse vector x by multiplying the elements
// by alpha and adding them to the corresponding elements in the SPA
// (SPA += alpha * x)
func (s *SPA) ScatterVec(x *Vector, alpha float64, ind *[]int) {
	s.Scatter(x.data, x.ind, alpha, ind)
}

// Scatter accumulates the sparse vector x by multiplying the elements by
// alpha and adding them to the corresponding elements in the SPA (SPA += alpha * x)
func (s *SPA) Scatter(x []float64, indx []int, alpha float64, ind *[]int) {
	for i, index := range indx {
		s.ScatterValue(x[i], index, alpha, ind)
	}
}

// ScatterValue accumulates a single value by multiplying the value by alpha
// and adding it to the corresponding element in the SPA (SPA += alpha * x)
func (s *SPA) ScatterValue(val float64, index int, alpha float64, ind *[]int) {
	if s.w[index] < s.generation+1 {
		s.w[index] = s.generation + 1
		*ind = append(*ind, index)
		s.y[index] = alpha * val
	} else {
		s.y[index] += alpha * val
	}
}

// AccumulateDense accumulates the dense vector x by multiplying the non-zero elements
// by alpha and adding them to the corresponding elements in the SPA (SPA += alpha * x)
// This is the dense version of the Scatter method for sparse vectors.
func (s *SPA) AccumulateDense(x []float64, alpha float64, ind *[]int) {
	for i, val := range x {
		if val != 0 {
			s.ScatterValue(val, i, alpha, ind)
		}
	}
}

// Gather gathers the non-zero values from the SPA and appends them to
// end of the supplied sparse vector.
func (s SPA) Gather(data *[]float64, ind *[]int) {
	for _, index := range (*ind)[s.nnz:] {
		*data = append(*data, s.y[index])
		//y[index] = 0
	}
}

// GatherAndZero gathers the non-zero values from the SPA and appends them
// to the end of the supplied sparse vector.  The SPA is also zeroed
// ready to start accumulating the next row/column vector.
func (s *SPA) GatherAndZero(data *[]float64, ind *[]int) {
	s.Gather(data, ind)

	s.nnz = len(*ind)
	s.generation++
}
