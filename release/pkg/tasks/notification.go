// Copyright (c) 2024-2025 Tigera, Inc. All rights reserved.

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

package tasks

import (
	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/release/internal/hashreleaseserver"
	"github.com/projectcalico/calico/release/internal/slack"
	"github.com/projectcalico/calico/release/internal/utils"
)

var product = utils.ProductName

// AnnounceHashrelease sends a slack notification for a new hashrelease.
func AnnounceHashrelease(cfg *slack.Config, hashrel *hashreleaseserver.Hashrelease, ciURL string) error {
	logrus.WithField("hashrelease", hashrel.Name).Info("Sending hashrelease announcement to Slack")
	msgData := &slack.HashreleaseMessageData{
		ReleaseName:        hashrel.Name,
		Product:            product,
		Stream:             hashrel.Stream,
		ProductVersion:     hashrel.ProductVersion,
		OperatorVersion:    hashrel.OperatorVersion,
		ReleaseType:        "hashrelease",
		CIURL:              ciURL,
		DocsURL:            hashrel.URL(),
		ImageScanResultURL: hashrel.ImageScanResultURL,
	}
	return slack.PostHashreleaseAnnouncement(cfg, msgData)
}
