// Copyright © 2016 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"fmt"

	"github.com/blevesearch/bleve/index/upsidedown"
	"github.com/spf13/cobra"
)

var docID string

// dumpCmd represents the dump command
var dumpCmd = &cobra.Command{
	Use:   "dump [index path]",
	Short: "dumps the contents of the index",
	Long:  `The dump command will dump (possibly a section of) the index.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		i, err := idx.Advanced()
		if err != nil {
			return fmt.Errorf("error getting index: %v", err)
		}
		r, err := i.Reader()
		if err != nil {
			return fmt.Errorf("error getting index reader: %v", err)
		}
		upsideDownReader, ok := r.(*upsidedown.IndexReader)
		if !ok {
			return fmt.Errorf("dump is only supported by index type upsidedown")
		}

		dumpChan := upsideDownReader.DumpAll()
		for rowOrErr := range dumpChan {
			switch rowOrErr := rowOrErr.(type) {
			case error:
				return fmt.Errorf("error dumping: %v", rowOrErr)
			case upsidedown.UpsideDownCouchRow:
				fmt.Printf("%v\n", rowOrErr)
				fmt.Printf("Key:   % -100x\nValue: % -100x\n\n", rowOrErr.Key(), rowOrErr.Value())
			}
		}
		return nil
	},
}

func init() {
	RootCmd.AddCommand(dumpCmd)
}
