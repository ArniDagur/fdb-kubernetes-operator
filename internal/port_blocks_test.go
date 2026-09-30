/*
 * port_blocks_test.go
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
	"net"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("port blocks", func() {
	DescribeTable("allocating a port block first-fit",
		func(size int, taken []PortInterval, expectedStart int, expectedOK bool) {
			// The range 20000-20019 has room for 20 ports.
			start, ok := AllocatePortBlock(20000, 20019, size, taken)
			Expect(ok).To(Equal(expectedOK))
			if expectedOK {
				Expect(start).To(Equal(expectedStart))
			}
		},
		Entry("an empty range starts at the beginning", 3, nil, 20000, true),
		Entry("the block goes right after a taken block at the start",
			3, []PortInterval{{Start: 20000, End: 20002}}, 20003, true),
		Entry("the block fills the first gap that is big enough",
			3,
			// Free: 20003-20005 (3 ports), 20009-20019.
			[]PortInterval{{Start: 20000, End: 20002}, {Start: 20006, End: 20008}},
			20003, true),
		Entry("a gap that is too small is skipped",
			5,
			// Free: 20003-20005 (3 ports, too small for 5), 20009-20019.
			[]PortInterval{{Start: 20000, End: 20002}, {Start: 20006, End: 20008}},
			20009, true),
		Entry("adjacent free gaps merge into one",
			5,
			// The released blocks 20003-20005 and 20006-20008 leave one free gap 20003-20008.
			[]PortInterval{{Start: 20000, End: 20002}, {Start: 20009, End: 20019}},
			20003, true),
		Entry(
			"taken intervals may be given in any order and may overlap",
			3,
			[]PortInterval{
				{Start: 20006, End: 20008},
				{Start: 20000, End: 20004},
				{Start: 20003, End: 20005},
			},
			20009,
			true,
		),
		Entry("a single excluded port blocks a gap",
			3,
			// 20001 is referenced by an exclusion, so the next free gap of 3 starts at 20002.
			[]PortInterval{{Start: 20001, End: 20001}},
			20002, true),
		Entry("taken ports outside of the range are ignored",
			3, []PortInterval{{Start: 19000, End: 19999}, {Start: 20020, End: 20030}}, 20000, true),
		Entry("a block that exactly fits the end of the range",
			3, []PortInterval{{Start: 20000, End: 20016}}, 20017, true),
		Entry("no gap is big enough",
			5,
			// Free: 20003-20005 and 20016-20019, both smaller than 5.
			[]PortInterval{{Start: 20000, End: 20002}, {Start: 20006, End: 20015}},
			0, false),
		Entry("the range is full", 3, []PortInterval{{Start: 20000, End: 20019}}, 0, false),
	)

	When("collecting the taken ports", func() {
		It(
			"should include blocks of process groups, blocks of pods and ports of IP:port exclusions",
			func() {
				taken := GetTakenPortIntervals(
					[]*fdbv1beta2.ProcessGroupStatus{
						{
							ProcessGroupID: "storage-1",
							PortBlock:      &fdbv1beta2.PortBlock{Start: 20000, ServersPerPod: 2},
						},
						{ProcessGroupID: "storage-2"},
					},
					[]*fdbv1beta2.PortBlock{{Start: 20010, ServersPerPod: 1}, nil},
					[]fdbv1beta2.ProcessAddress{
						{IPAddress: net.ParseIP("10.0.0.1")},
						{IPAddress: net.ParseIP("10.0.0.1"), Port: 20020},
						{StringAddress: fdbv1beta2.FDBLocalityExclusionPrefix + ":storage-3"},
					},
				)

				Expect(taken).To(ConsistOf(
					PortInterval{Start: 20000, End: 20004},
					PortInterval{Start: 20010, End: 20012},
					PortInterval{Start: 20020, End: 20020},
				))
			},
		)
	})
})
