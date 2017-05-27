// Copyright 2016 The Gosl Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// package opt implements routines for solving optimisation problems
package opt

import (
	"math"

	"github.com/cpmech/gosl/chk"
	"github.com/cpmech/gosl/fun"
	"github.com/cpmech/gosl/io"
	"github.com/cpmech/gosl/la"
)

// LinIpm implements the interior-point methods for linear programming problems
//  Solve:
//          min cᵀx   s.t.   Aᵀx = b, x ≥ 0
//           x
//
//  or the dual problem:
//
//          max bᵀλ   s.t.   Aᵀλ + s = c, s ≥ 0
//           λ
type LinIpm struct {

	// problem
	A *la.CCMatrix // [Nl][nx]
	B []float64    // [Nl]
	C []float64    // [nx]

	// constants
	NmaxIt int     // max number of iterations
	Tol    float64 // tolerance ϵ for stopping iterations

	// dimensions
	Nx int // number of x
	Nl int // number of λ
	Ny int // number of y = nx + ns + nl = 2 * nx + nl

	// solution vector
	Y   []float64 // y := [x, λ, s]
	X   []float64 // subset of y
	L   []float64 // subset of y
	S   []float64 // subset of y
	Mdy []float64 // -Δy
	Mdx []float64 // subset of Mdy == -Δx
	Mdl []float64 // subset of Mdy == -Δλ
	Mds []float64 // subset of Mdy == -Δs

	// affine solution
	R  []float64   // residual
	Rx []float64   // subset of R
	Rl []float64   // subset of R
	Rs []float64   // subset of R
	J  *la.Triplet // [ny][ny] Jacobian matrix

	// linear solver
	Lis la.LinSol // linear solver
}

// Free frees allocated memory
func (o *LinIpm) Free() {
	o.Lis.Free()
}

// Init initialises LinIpm
func (o *LinIpm) Init(A *la.CCMatrix, b, c []float64, prms fun.Params) {

	// problem
	o.A, o.B, o.C = A, b, c

	// constants
	o.NmaxIt = 50
	o.Tol = 1e-8
	for _, p := range prms {
		switch p.N {
		case "nmaxit":
			o.NmaxIt = int(p.V)
		}
	}

	// dimensions
	o.Nx = len(o.C)
	o.Nl = len(o.B)
	o.Ny = 2*o.Nx + o.Nl
	ix, jx := 0, o.Nx
	il, jl := o.Nx, o.Nx+o.Nl
	is, js := o.Nx+o.Nl, o.Ny

	// solution vector
	o.Y = make([]float64, o.Ny)
	o.X = o.Y[ix:jx]
	o.L = o.Y[il:jl]
	o.S = o.Y[is:js]
	o.Mdy = make([]float64, o.Ny)
	o.Mdx = o.Mdy[ix:jx]
	o.Mdl = o.Mdy[il:jl]
	o.Mds = o.Mdy[is:js]

	// affine solution
	o.R = make([]float64, o.Ny)
	o.Rx = o.R[ix:jx]
	o.Rl = o.R[il:jl]
	o.Rs = o.R[is:js]
	o.J = new(la.Triplet)
	nnz := 2*o.Nl*o.Nx + 3*o.Nx
	o.J.Init(o.Ny, o.Ny, nnz)

	// linear solver
	o.Lis = la.GetSolver("umfpack")
}

// Solve solves linear programming problem
func (o *LinIpm) Solve(verbose bool) (err error) {

	// starting point
	AAt := la.MatAlloc(o.Nl, o.Nl)         // A*Aᵀ
	d := make([]float64, o.Nl)             // inv(AAt) * b
	e := make([]float64, o.Nl)             // A * c
	la.SpMatMatTrMul(AAt, 1, o.A)          // AAt := A*Aᵀ
	la.SpMatVecMul(e, 1, o.A, o.C)         // e := A * c
	la.SPDsolve2(d, o.L, AAt, o.B, e)      // d := inv(AAt) * b  and  L := inv(AAt) * e
	la.SpMatTrVecMul(o.X, 1, o.A, d)       // x := Aᵀ * d
	la.VecCopy(o.S, 1, o.C)                // s := c
	la.SpMatTrVecMulAdd(o.S, -1, o.A, o.L) // s -= Aᵀλ
	xmin := o.X[0]
	smin := o.S[0]
	for i := 1; i < o.Nx; i++ {
		xmin = min(xmin, o.X[i])
		smin = min(smin, o.S[i])
	}
	δx := max(-1.5*xmin, 0)
	δs := max(-1.5*smin, 0)
	var xdots, xsum, ssum float64
	for i := 0; i < o.Nx; i++ {
		o.X[i] += δx
		o.S[i] += δs
		xdots += o.X[i] * o.S[i]
		xsum += o.X[i]
		ssum += o.S[i]
	}
	δx = 0.5 * xdots / ssum
	δs = 0.5 * xdots / xsum
	for i := 0; i < o.Nx; i++ {
		o.X[i] += δx
		o.S[i] += δs
	}

	// constants for linear solver
	symmetric := false
	timing := false

	// auxiliary
	I := o.Nx + o.Nl

	// control variables
	var μ, σ float64     // μ and σ
	var xrmin float64    // min{ x_i / (-Δx_i) } (x_ratio_minimum)
	var srmin float64    // min{ s_i / (-Δs_i) } (s_ratio_minimum)
	var αpa float64      // α_prime_affine
	var αda float64      // α_dual_affine
	var μaff float64     // μ_affine
	var ctx, btl float64 // cᵀx and bᵀl

	// message
	if verbose {
		io.Pf("%3s%16s%16s\n", "it", "f(x)", "error")
	}

	// perform iterations
	it := 0
	for it = 0; it < o.NmaxIt; it++ {

		// compute residual
		la.SpMatTrVecMul(o.Rx, 1, o.A, o.L) // rx := Aᵀλ
		la.SpMatVecMul(o.Rl, 1, o.A, o.X)   // rλ := A x
		ctx, btl, μ = 0, 0, 0
		for i := 0; i < o.Nx; i++ {
			o.Rx[i] += o.S[i] - o.C[i]
			o.Rs[i] = o.X[i] * o.S[i]
			ctx += o.C[i] * o.X[i]
			μ += o.X[i] * o.S[i]
		}
		for i := 0; i < o.Nl; i++ {
			o.Rl[i] -= o.B[i]
			btl += o.B[i] * o.L[i]
		}
		μ /= float64(o.Nx)

		// check convergence
		lerr := math.Abs(ctx-btl) / (1.0 + math.Abs(ctx))
		if verbose {
			fx := la.VecDot(o.C, o.X)
			io.Pf("%3d%16.8e%16.8e\n", it, fx, lerr)
		}
		if lerr < o.Tol {
			break
		}

		// assemble Jacobian
		o.J.Start()
		o.J.PutCCMatAndMatT(o.A)
		for i := 0; i < o.Nx; i++ {
			o.J.Put(i, I+i, 1.0)
			o.J.Put(I+i, i, o.S[i])
			o.J.Put(I+i, I+i, o.X[i])
		}

		// solve linear system
		if it == 0 {
			err = o.Lis.InitR(o.J, symmetric, false, timing)
			if err != nil {
				return
			}
		}
		err = o.Lis.Fact()
		if err != nil {
			return
		}
		err = o.Lis.SolveR(o.Mdy, o.R, false) // mdy := inv(J) * R
		if err != nil {
			return
		}

		// control variables
		xrmin, srmin = o.calc_min_ratios()
		αpa = min(1, xrmin)
		αda = min(1, srmin)
		μaff = 0
		for i := 0; i < o.Nx; i++ {
			μaff += (o.X[i] - αpa*o.Mdx[i]) * (o.S[i] - αda*o.Mds[i])
		}
		μaff /= float64(o.Nx)
		σ = math.Pow(μaff/μ, 3)

		// update residual
		for i := 0; i < o.Nx; i++ {
			o.Rs[i] += o.Mdx[i]*o.Mds[i] - σ*μ
		}

		// solve linear system again
		err = o.Lis.SolveR(o.Mdy, o.R, false) // mdy := inv(J) * R
		if err != nil {
			return
		}

		// step lengths
		xrmin, srmin = o.calc_min_ratios()
		αpa = min(1, 0.99*xrmin)
		αda = min(1, 0.99*srmin)

		// update
		for i := 0; i < o.Nx; i++ {
			o.X[i] -= αpa * o.Mdx[i]
			o.S[i] -= αda * o.Mds[i]
		}
		for i := 0; i < o.Nl; i++ {
			o.L[i] -= αda * o.Mdl[i]
		}
	}

	// check convergence
	if it == o.NmaxIt {
		err = chk.Err("iterations did not converge")
	}
	return
}

func (o *LinIpm) calc_min_ratios() (xrmin, srmin float64) {
	firstxrmin, firstsrmin := true, true
	for i := 0; i < o.Nx; i++ {
		if o.Mdx[i] > 0 {
			if firstxrmin {
				xrmin = o.X[i] / o.Mdx[i]
				firstxrmin = false
			} else {
				xrmin = min(xrmin, o.X[i]/o.Mdx[i])
			}
		}
		if o.Mds[i] > 0 {
			if firstsrmin {
				srmin = o.S[i] / o.Mds[i]
				firstsrmin = false
			} else {
				srmin = min(srmin, o.S[i]/o.Mds[i])
			}
		}
	}
	return
}
