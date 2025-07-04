// SPDX-License-Identifier: Apache-2.0
//
// Copyright (C) 2021 Renesas Electronics Corporation.
// Copyright (C) 2021 EPAM Systems, Inc.
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

package unitstatushandler

import (
	"time"

	"github.com/aosedge/aos_common/aoserrors"
	"github.com/aosedge/aos_common/api/cloudprotocol"
	log "github.com/sirupsen/logrus"
)

/***********************************************************************************************************************
 * Consts
 **********************************************************************************************************************/

const (
	daysInWeek = 7
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

func validateTimetable(timetable []cloudprotocol.TimetableEntry) (err error) {
	if len(timetable) == 0 {
		return aoserrors.New("timetable is empty")
	}

	for _, entry := range timetable {
		if entry.DayOfWeek > 7 || entry.DayOfWeek < 1 {
			return aoserrors.New("invalid day of week value")
		}

		if len(entry.TimeSlots) == 0 {
			return aoserrors.New("no time slots")
		}

		for _, slot := range entry.TimeSlots {
			if year, month, day := slot.Start.Date(); year != 0 || month != 1 || day != 1 {
				return aoserrors.New("start value should contain only time")
			}

			if year, month, day := slot.End.Date(); year != 0 || month != 1 || day != 1 {
				return aoserrors.New("end value should contain only time")
			}

			if slot.Start.After(slot.End.Time) || slot.Start.Equal(slot.End.Time) {
				return aoserrors.New("start value should be before end value")
			}
		}
	}

	return nil
}

func getAvailableTimetableTime(
	fromDate time.Time, timetable []cloudprotocol.TimetableEntry,
) (availableTime time.Duration, err error) {
	defer func() {
		if err == nil {
			log.WithFields(log.Fields{
				"fromDate": fromDate, "availableTime": availableTime,
			}).Debug("Get available timetable time")
		}
	}()

	if err = validateTimetable(timetable); err != nil {
		return availableTime, err
	}

	timetableMap := make(map[time.Weekday][]cloudprotocol.TimeSlot)

	for _, entry := range timetable {
		dayOfWeek := time.Weekday(entry.DayOfWeek)

		if dayOfWeek == daysInWeek {
			dayOfWeek = 0
		}

		timetableMap[dayOfWeek] = append(timetableMap[dayOfWeek], entry.TimeSlots...)
	}

	for i := 0; i <= daysInWeek; i++ {
		curWeekday := (fromDate.Weekday() + time.Weekday(i)) % daysInWeek

		nearestDuration := time.Duration(1<<63 - 1)

		for _, slot := range timetableMap[curWeekday] {
			startTime := time.Date(fromDate.Year(), fromDate.Month(), fromDate.Day(),
				slot.Start.Hour(), slot.Start.Minute(), slot.Start.Second(), slot.Start.Nanosecond(),
				time.Local).Add(24 * time.Duration(i) * time.Hour) //nolint:gosmopolitan
			endTime := time.Date(fromDate.Year(), fromDate.Month(), fromDate.Day(),
				slot.End.Hour(), slot.End.Minute(), slot.End.Second(), slot.End.Nanosecond(),
				time.Local).Add(24 * time.Duration(i) * time.Hour) //nolint:gosmopolitan

			if (startTime.Before(fromDate) || startTime.Equal(fromDate)) && endTime.After(fromDate) {
				return 0, nil
			}

			if endTime.Before(fromDate) || endTime.Equal(fromDate) {
				continue
			}

			if startTime.Sub(fromDate) < nearestDuration {
				nearestDuration = startTime.Sub(fromDate)
			}

			return nearestDuration, nil
		}
	}

	return availableTime, aoserrors.New("no available time")
}
