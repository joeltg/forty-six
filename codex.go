package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	badger "github.com/dgraph-io/badger"
	ld "github.com/piprate/json-gold/ld"
)

// Codex is a map of refs. A codex is always relative to a specific variable.
type Codex struct {
	Constraint []*Reference            // an almost-always empty list of refs that include the codex's variable more than once
	Single     []*Reference            // list of refs that include the variable once, and two known values
	Double     map[string][]*Reference // list of refs that include the variable once, one unknown value, and one known value
	Norm       uint64                  // The sum of squares of key counts of references.
	Length     int                     // The total number of references
	Count      uint64
}

func (codex *Codex) String() string {
	var val string
	val += fmt.Sprintf("Constraint: %s\n", referenceSet(codex.Constraint).String())
	val += fmt.Sprintf("Singles: %s\n", referenceSet(codex.Single).String())
	val += fmt.Sprintln("Doubles:")
	for id, refs := range codex.Double {
		val += fmt.Sprintf("  %s: %s\n", id, referenceSet(refs).String())
	}
	val += fmt.Sprintf("Norm: %d\n", codex.Norm)
	val += fmt.Sprintf("Length: %d\n", codex.Length)
	return val
}

func (codex *Codex) close() {
	for _, ref := range codex.Single {
		ref.close()
	}
	for _, refs := range codex.Double {
		for _, ref := range refs {
			ref.close()
		}
	}
}

// A CodexMap associates ids with Codex maps.
type CodexMap struct {
	Index map[string]*Codex
	Slice []string
}

// Sort interface functions
func (c *CodexMap) Len() int      { return len(c.Slice) }
func (c *CodexMap) Swap(a, b int) { c.Slice[a], c.Slice[b] = c.Slice[b], c.Slice[a] }
func (c *CodexMap) Less(a, b int) bool {
	A, B := c.Index[c.Slice[a]], c.Index[c.Slice[b]]
	// return (float32(A.Norm) / float32(A.Length)) < (float32(B.Norm) / float32(B.Length))
	return A.Count > B.Count
}

// GetCodex retrieves an Codex or creates one if it doesn't exist.
func (c *CodexMap) GetCodex(id string) *Codex {
	if c.Index == nil {
		c.Index = map[string]*Codex{}
	}
	codex, has := c.Index[id]
	if !has {
		codex = &Codex{}
		c.Index[id] = codex
		c.Slice = append(c.Slice, id)
	}
	return codex
}

// InsertDouble into codex.Double
func (c *CodexMap) InsertDouble(a string, b string, ref *Reference) {
	codex := c.GetCodex(a)
	if codex.Double == nil {
		codex.Double = map[string][]*Reference{}
	}
	if refs, has := codex.Double[b]; has {
		codex.Double[b] = append(refs, ref)
	} else {
		codex.Double[b] = []*Reference{ref}
	}

}

// This is re-used between Major, Minor, and Index keys
func getCount(count []byte, key []byte, txn *badger.Txn) ([]byte, uint64, error) {
	item, err := txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return nil, 0, nil
	} else if err != nil {
		return nil, 0, err
	} else if count, err = item.ValueCopy(count); err != nil {
		return nil, 0, err
	} else {
		return count, binary.BigEndian.Uint64(count), nil
	}
}

// TODO: Make Sure you close the assignment.Present iterators some day
func (c *CodexMap) getAssignmentTree(txn *badger.Txn) ([]string, map[string]*Assignment, error) {
	var err error
	var major bool
	var key []byte
	count := make([]byte, 8)

	// Update the counts before sorting the codex map
	for _, codex := range c.Index {
		codex.Count = 0
		for _, ref := range codex.Single {
			key, major = ref.assembleCountKey(nil, major)
			ref.Cursor = &Cursor{}
			count, ref.Cursor.Count, err = getCount(count, key, txn)
			if err != nil {
				return nil, nil, err
			} else if ref.Cursor.Count == 0 {
				return nil, nil, fmt.Errorf("Single reference count of zero: %s", ref.String())
			}
			codex.Count += ref.Cursor.Count
			codex.Norm += ref.Cursor.Count * ref.Cursor.Count
			codex.Length++
		}
		for _, refs := range codex.Double {
			for _, ref := range refs {
				key, major = ref.assembleCountKey(nil, major)
				ref.Cursor = &Cursor{}
				count, ref.Cursor.Count, err = getCount(count, key, txn)
				if err != nil {
					return nil, nil, err
				} else if ref.Cursor.Count == 0 {
					return nil, nil, fmt.Errorf("Double reference count of zero: %s", ref.String())
				}
				codex.Count += ref.Cursor.Count
				codex.Norm += ref.Cursor.Count * ref.Cursor.Count
				codex.Length++
			}
		}
	}

	// fmt.Println("sorted values:")
	// printCodexMap(c)
	// Now sort the codex map
	sort.Stable(c)
	// fmt.Println("the codex map has been sorted", c.Slice)

	index := map[string]*Assignment{}
	indexMap := map[string]int{}
	for i, id := range c.Slice {
		indexMap[id] = i

		codex := c.Index[id]

		index[id] = &Assignment{
			Constraint: referenceSet(codex.Constraint),
			Present:    referenceSet(codex.Single),
			Past:       &Past{},
			Future:     map[string]referenceSet{},
		}

		deps := map[int]int{}
		past := index[id].Past
		for dep, refs := range codex.Double {
			if j, has := indexMap[dep]; has {
				past.Push(dep, j, refs)
				for k, ref := range refs {
					ref.Cursor.ID = dep
					ref.Cursor.Index = k
					past.insertIndex(dep, k, len(past.Cursors))
					past.Cursors = append(past.Cursors, ref.Cursor)
				}
				if j > deps[j] {
					deps[j] = j
				}
				for _, k := range index[dep].Dependencies {
					if j > deps[k] {
						deps[k] = j
					}
				}
			} else {
				index[id].Future[dep] = refs
			}
		}

		index[id].Past.sortOrder()

		// cursors := make(CursorSet, 0, cursorCount)
		// fmt.Println(len(index[id].Past.Slice), index[id].Past.Slice)
		// fmt.Println("and cursorCount", cursorCount)
		// for _, dep := range index[id].Past.Slice {
		// 	fmt.Println("trying for id", id)
		// }

		index[id].Dependencies = make([]int, 0, len(deps))
		for j := range deps {
			index[id].Dependencies = append(index[id].Dependencies, j)
		}
		sort.Sort(index[id].Dependencies)

		// fmt.Println("about to set value root for", id)
		index[id].setValueRoot(txn)
		if index[id].ValueRoot == nil {
			return nil, nil, fmt.Errorf("Assignment's static intersect is empty: %v", index[id])
		}
	}
	// return slice, index, nil
	// fmt.Println("returning slice", c.Slice)
	return c.Slice, index, nil
}

func (c *CodexMap) close() {
	for _, id := range c.Slice {
		c.Index[id].close()
	}
}

func makeReference(graph string, index int, permutation byte, m ld.Node, n ld.Node) *Reference {
	return &Reference{graph, index, permutation, m, n, &Cursor{}, nil}
}

func getInitalCodexMap(dataset *ld.RDFDataset) ([]*Reference, *CodexMap, error) {
	constants := []*Reference{}
	codexMap := &CodexMap{}
	for graph, quads := range dataset.Graphs {
		for index, quad := range quads {
			var a, b, c string
			blankA, A := quad.Subject.(*ld.BlankNode)
			if A {
				a = blankA.Attribute
			}
			blankB, B := quad.Predicate.(*ld.BlankNode)
			if B {
				b = blankB.Attribute
			}
			blankC, C := quad.Object.(*ld.BlankNode)
			if C {
				c = blankC.Attribute
			}
			if !A && !B && !C {
				ref := makeReference(graph, index, constantPermutation, nil, nil)
				constants = append(constants, ref)
			} else if (A && !B && !C) || (!A && B && !C) || (!A && !B && C) {
				ref := &Reference{Graph: graph, Index: index}
				if A {
					ref.Permutation = 0
					ref.M = quad.Predicate
					ref.N = quad.Object
				} else if B {
					ref.Permutation = 1
					ref.M = quad.Object
					ref.N = quad.Subject
				} else if C {
					ref.Permutation = 2
					ref.M = quad.Subject
					ref.N = quad.Predicate
				}
				pivot := a + b + c
				codex := codexMap.GetCodex(pivot)
				codex.Single = append(codex.Single, ref)
			} else if A && B && !C {
				if a == b {
					ref := makeReference(graph, index, permutationAB, nil, quad.Object)
					codex := codexMap.GetCodex(a)
					codex.Constraint = append(codex.Constraint, ref)
				} else {
					refA := makeReference(graph, index, permutationA, blankB, quad.Object)
					refB := makeReference(graph, index, permutationB, quad.Object, blankA)
					codexMap.InsertDouble(a, b, refA)
					codexMap.InsertDouble(b, a, refB)
					refA.Dual, refB.Dual = refB, refA
				}
			} else if A && !B && C {
				if c == a {
					ref := makeReference(graph, index, permutationCA, nil, quad.Predicate)
					codex := codexMap.GetCodex(c)
					codex.Constraint = append(codex.Constraint, ref)
				} else {
					refA := makeReference(graph, index, permutationA, quad.Predicate, blankC)
					refC := makeReference(graph, index, permutationC, blankA, quad.Predicate)
					codexMap.InsertDouble(a, c, refA)
					codexMap.InsertDouble(c, a, refC)
					refA.Dual, refC.Dual = refC, refA
				}
			} else if !A && B && C {
				if b == c {
					ref := makeReference(graph, index, permutationBC, nil, quad.Subject)
					codex := codexMap.GetCodex(b)
					codex.Constraint = append(codex.Constraint, ref)
				} else {
					refB := makeReference(graph, index, permutationB, blankC, quad.Subject)
					refC := makeReference(graph, index, permutationC, quad.Subject, blankB)
					codexMap.InsertDouble(b, c, refB)
					codexMap.InsertDouble(c, b, refC)
					refB.Dual, refC.Dual = refC, refB
				}
			} else if A && B && C {
				return nil, nil, errors.New("Cannot handle all-blank triple")
			}
		}
	}
	return constants, codexMap, nil
}
