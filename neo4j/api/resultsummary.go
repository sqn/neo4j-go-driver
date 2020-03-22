/*
 * Copyright (c) 2002-2020 "Neo4j,"
 * Neo4j Sweden AB [http://neo4j.com]
 *
 * This file is part of Neo4j.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package api

type ResultSummary interface {
	// Server returns basic information about the server where the statement is carried out.
	Server() ServerInfo
	// Statement returns statement that has been executed.
	Statement() Statement
	// StatementType returns type of statement that has been executed.
	//StatementType() StatementType
	// Counters returns statistics counts for the statement.
	//Counters() Counters
	// Plan returns statement plan for the executed statement if available, otherwise null.
	//Plan() Plan
	// Profile returns profiled statement plan for the executed statement if available, otherwise null.
	//Profile() ProfiledPlan
	// Notifications returns a slice of notifications produced while executing the statement.
	// The list will be empty if no notifications produced while executing the statement.
	//Notifications() []Notification
	// ResultAvailableAfter returns the time it took for the server to make the result available for consumption.
	//ResultAvailableAfter() time.Duration
	// ResultConsumedAfter returns the time it took the server to consume the result.
	//ResultConsumedAfter() time.Duration
}

type Statement interface {
	// Text returns the statement's text.
	Text() string
	// Params returns the statement's parameters.
	Params() map[string]interface{}
}

// ServerInfo contains basic information of the server.
type ServerInfo interface {
	// Address returns the address of the server.
	//Address() string
	// Version returns the version of Neo4j running at the server.
	Version() string
}
