/**
 * Copyright (c) 2024 Yunshan Networks
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

package cache

import (
	"sync"

	"github.com/deepflowio/deepflow/message/controller"
	"github.com/deepflowio/deepflow/server/controller/db/mysql"
)

type LabelKey struct {
	Name  string
	Value string
}

func NewLabelKey(name, value string) LabelKey {
	return LabelKey{
		Name:  name,
		Value: value,
	}
}

type label struct {
	idToKey sync.Map
	keyToID sync.Map
}

func newLabel() *label {
	return &label{}
}

func (l *label) GetIDByKey(key LabelKey) (int, bool) {
	if item, ok := l.keyToID.Load(key); ok {
		return item.(int), true
	}
	return 0, false
}

func (l *label) GetKeyByID(id int) (LabelKey, bool) {
	if item, ok := l.idToKey.Load(id); ok {
		return item.(LabelKey), true
	}
	return LabelKey{}, false
}

func (l *label) Add(batch []*controller.PrometheusLabel) {
	for _, item := range batch {
		k := NewLabelKey(item.GetName(), item.GetValue())
		l.keyToID.Store(k, int(item.GetId()))
		l.idToKey.Store(int(item.GetId()), k)
	}
}

func (l *label) refresh(args ...interface{}) error {
	ls, err := l.load()
	if err != nil {
		return err
	}
	for _, item := range ls {
		k := NewLabelKey(item.Name, item.Value)
		l.keyToID.Store(k, item.ID)
		l.idToKey.Store(item.ID, k)
	}
	return nil
}

func (l *label) load() ([]*mysql.PrometheusLabel, error) {
	var labels []*mysql.PrometheusLabel
	err := mysql.Db.Find(&labels).Error
	return labels, err
}
