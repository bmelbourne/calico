// Copyright (c) 2019 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/projectcalico/calico/pkg/buildinfo"
)

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Prints the version and exits",
	Run: func(cmd *cobra.Command, args []string) {
		version := "Version:            " + buildinfo.Version + "\n" +
			"Full git commit ID: " + buildinfo.GitRevision + "\n" +
			"Build date:         " + buildinfo.BuildDate + "\n"
		fmt.Print(version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
