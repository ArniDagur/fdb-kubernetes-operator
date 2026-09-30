/*
 * port_blocks.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2026 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package internal

import (
	"slices"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
)

// PortInterval is an inclusive range of ports.
type PortInterval struct {
	// Start is the first port of the interval.
	Start int
	// End is the last port of the interval.
	End int
}

// GetTakenPortIntervals returns the ports that new port blocks must not use: the blocks of all process groups, the
// blocks of all existing pods, and every port that appears in an IP:port exclusion.
func GetTakenPortIntervals(
	processGroups []*fdbv1beta2.ProcessGroupStatus,
	podBlocks []*fdbv1beta2.PortBlock,
	exclusions []fdbv1beta2.ProcessAddress,
) []PortInterval {
	taken := make([]PortInterval, 0, len(processGroups)+len(podBlocks)+len(exclusions))
	for _, processGroup := range processGroups {
		if processGroup.PortBlock == nil {
			continue
		}

		taken = append(taken, PortInterval{
			Start: processGroup.PortBlock.Start,
			End:   processGroup.PortBlock.End(),
		})
	}

	for _, block := range podBlocks {
		if block == nil {
			continue
		}

		taken = append(taken, PortInterval{Start: block.Start, End: block.End()})
	}

	for _, exclusion := range exclusions {
		if exclusion.Port == 0 {
			continue
		}

		taken = append(taken, PortInterval{Start: exclusion.Port, End: exclusion.Port})
	}

	return taken
}

// AllocatePortBlock returns the first port of the lowest gap in the inclusive range [rangeStart, rangeEnd] that has
// room for size ports and does not overlap any of the taken intervals. The second return value is false if no gap is
// big enough.
func AllocatePortBlock(rangeStart int, rangeEnd int, size int, taken []PortInterval) (int, bool) {
	sorted := slices.Clone(taken)
	slices.SortFunc(sorted, func(a, b PortInterval) int {
		return a.Start - b.Start
	})

	candidate := rangeStart
	for _, interval := range sorted {
		if interval.End < candidate {
			continue
		}

		if candidate+size-1 < interval.Start {
			break
		}

		candidate = interval.End + 1
	}

	if candidate+size-1 > rangeEnd {
		return 0, false
	}

	return candidate, true
}
