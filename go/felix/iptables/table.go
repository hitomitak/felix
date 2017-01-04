// Copyright (c) 2016-2017 Tigera, Inc. All rights reserved.
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

package iptables

import (
	"bytes"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/projectcalico/felix/go/felix/set"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const (
	MaxChainNameLength = 28
)

var (
	// List of all the top-level kernel-created chains by iptables table.
	tableToKernelChains = map[string][]string{
		"filter": []string{"INPUT", "FORWARD", "OUTPUT"},
		"nat":    []string{"PREROUTING", "INPUT", "OUTPUT", "POSTROUTING"},
		"mangle": []string{"PREROUTING", "INPUT", "FORWARD", "OUTPUT", "POSTROUTING"},
		"raw":    []string{"PREROUTING", "OUTPUT"},
	}

	// chainCreateRegexp matches iptables-save output lines for chain forward reference lines.
	// It captures the name of the chain.
	chainCreateRegexp = regexp.MustCompile(`^:(\S+)`)
	// appendRegexp matches an iptables-save output line for an append operation.
	appendRegexp = regexp.MustCompile(`^-A (\S+)`)
)

// Table represents a single one of the iptables tables i.e. "raw", "nat", "filter", etc.  It
// caches the desired state of that table, then attempts to bring it into sync when Apply() is
// called.
//
// API Model
//
// Table supports two classes of operation:  "rule insertions" and "full chain updates".
//
// As the name suggests, rule insertions allow for inserting one or more rules into a pre-existing
// chain.  Rule insertions are intended to be used to hook kernel chains (such as "FORWARD") in
// order to direct them to a Felix-owned chain.  It is important to minimise the use of rule
// insertions because the top-level chains are shared resources, which can be modified by other
// applications.  In addition, rule insertions are harder to clean up after an upgrade to a new
// version of Felix (because we need a way to recognise our rules in a crowded chain).
//
// Full chain updates replace the entire contents of a Felix-owned chain with a new set of rules.
// Limiting the operation to "replace whole chain" in this way significantly simplifies the API.
// Although the API operates on full chains, the dataplane write logic tries to avoid rewriting
// a whole chain if only part of it has changed (this was not the case in Felix 1.4).  This
// prevents iptables counters from being reset unnecessarily.
//
// In either case, the actual dataplane updates are deferred until the next call to Apply() so
// chain updates and insertions may occur in any order as long as they are consistent (i.e. there
// are no references to non-existent chains) by the time Apply() is called.
//
// Design
//
// We had several goals in designing the iptables machinery in 2.0.0:
//
// (1) High performance. Felix needs to handle high churn of endpoints and rules.
//
// (2) Ability to restore rules, even if other applications accidentally break them: we found that
// other applications sometimes misuse iptables-save and iptables-restore to do a read, modify,
// write cycle. That behaviour is not safe under concurrent modification.
//
// (3) Avoid rewriting rules that haven't changed so that we don't reset iptables counters.
//
// (4) Avoid parsing iptables commands (for example, the output from iptables/iptables-save).
// This is very hard to do robustly because iptables rules do not necessarily round-trip through
// the kernel in the same form.  In addition, the format could easily change due to changes or
// fixes in the iptables/iptables-save command.
//
// (5) Support for graceful restart.  I.e. deferring potentially incorrect updates until we're
// in-sync with the datastore.  For example, if we have 100 endpoints on a host, after a restart
// we don't want to write a "dispatch" chain when we learn about the first endpoint (possibly
// replacing an existing one that had all 100 endpoints in place and causing traffic to glitch);
// instead, we want to defer until we've seen all 100 and then do the write.
//
// (6) Improved handling of rule inserts vs Felix 1.4.x.  Previous versions of Felix sometimes
// inserted special-case rules that were not marked as Calico rules in any sensible way making
// cleanup of those rules after an upgrade difficult.
//
// Implementation
//
// For high performance (goal 1), we use iptables-restore to do bulk updates to iptables.  This is
// much faster than individual iptables calls.
//
// To allow us to restore rules after they are clobbered by another process (goal 2), we cache
// them at this layer.  This means that we don't need a mechanism to ask the other layers of Felix
// to do a resync.  Note: Table doesn't start a thread of its own so it relies on the main event
// loop to trigger any dataplane resync polls.
//
// There is tension between goals 3 and 4.  In order to avoid full rewrites (goal 3), we need to
// know what rules are in place, but we also don't want to parse them to find out (goal 4)!  As
// a compromise, we deterministically calculate an ID for each rule and store it in an iptables
// comment.  Then, when we want to know what rules are in place, we _do_ parse the output from
// iptables-save, but only to read back the rule IDs.  That limits the amount of parsing we need
// to do and keeps it manageable/robust.
//
// To support graceful restart (goal 5), we defer updates to the dataplane until Apply() is called,
// then we do an atomic update using iptables-restore.  As long as the first Apply() call is
// after we're in sync, the dataplane won't be touched until the right time.  Felix 1.4.x had a
// more complex mechanism to support partial updates during the graceful restart period but
// Felix 2.0.0 resyncs so quickly that the added complexity is not justified.
//
// To make it easier to manage rule insertions (goal 6), we add rule IDs to those too.  With
// rule IDs in place, we can easily distinguish Calico rules from non-Calico rules without needing
// to know exactly which rules to expect.  To deal with cleanup after upgrade from older versions
// that did not write rule IDs, we support special-case regexes to detect our old rules.
//
// Thread safety
//
// Table doesn't do any internal synchronization, its methods should only be called from one
// thread.  To avoid conflicts in the dataplane itself, there should only be one instance of
// Table for each iptable table in an application.
type Table struct {
	Name      string
	IPVersion uint8

	// chainToInsertedRules maps from chain name to a list of rules to be inserted at the start
	// of that chain.  Rules are written with rule hash comments.  The Table cleans up inserted
	// rules with unknown hashes.
	chainToInsertedRules map[string][]Rule
	dirtyInserts         set.Set

	// chainToRuleFragments contains the desired state of our iptables chains, indexed by
	// chain name.  The values are slices of iptables fragments, such as
	// "--match foo --jump DROP" (i.e. omitting the action and chain name, which are calculated
	// as needed).
	chainNameToChain map[string]*Chain
	dirtyChains      set.Set

	inSyncWithDataPlane bool

	// chainToDataplaneHashes contains the rule hashes that we think are in the dataplane.
	// it is updated when we write to the dataplane but it can also be read back and compared
	// to what we calculate from chainToContents.
	chainToDataplaneHashes map[string][]string

	// hashCommentPrefix holds the prefix that we prepend to our rule-tracking hashes.
	hashCommentPrefix string
	// hashCommentRegexp matches the rule-tracking comment, capturing the rule hash.
	hashCommentRegexp *regexp.Regexp
	// ourChainsRegexp matches the names of chains that are "ours", i.e. start with one of our
	// prefixes.
	ourChainsRegexp *regexp.Regexp
	// oldInsertRegexp matches inserted rules from old pre rule-hash versions of felix.
	oldInsertRegexp *regexp.Regexp

	iptablesRestoreCmd string
	iptablesSaveCmd    string

	logCxt *log.Entry

	// Factory for making commands, used by UTs to shim exec.Command().
	newCmd cmdFactory
}

func NewTable(
	name string,
	ipVersion uint8,
	historicChainPrefixes []string,
	hashPrefix string,
	extraCleanupRegexPattern string,
) *Table {
	return NewTableWithShims(
		name,
		ipVersion,
		historicChainPrefixes,
		hashPrefix,
		extraCleanupRegexPattern,
		newRealCmd,
	)
}

// NewTableWithShims is a test constructor, allowing exec.Command to be shimmed.
func NewTableWithShims(
	name string,
	ipVersion uint8,
	historicChainPrefixes []string,
	hashPrefix string,
	extraCleanupRegexPattern string,
	newCmd cmdFactory,
) *Table {
	// Calculate the regex used to match the hash comment.  The comment looks like this:
	// --comment "cali:abcd1234_-".
	hashCommentRegexp := regexp.MustCompile(`--comment "?` + hashPrefix + `([a-zA-Z0-9_-]+)"?`)
	ourChainsPattern := "^(" + strings.Join(historicChainPrefixes, "|") + ")"
	ourChainsRegexp := regexp.MustCompile(ourChainsPattern)

	oldInsertRegexpParts := []string{}
	for _, prefix := range historicChainPrefixes {
		part := fmt.Sprintf("(?:-j|--jump) %s", prefix)
		oldInsertRegexpParts = append(oldInsertRegexpParts, part)
	}
	if extraCleanupRegexPattern != "" {
		oldInsertRegexpParts = append(oldInsertRegexpParts, extraCleanupRegexPattern)
	}
	oldInsertPattern := strings.Join(oldInsertRegexpParts, "|")
	oldInsertRegexp := regexp.MustCompile(oldInsertPattern)

	// Pre-populate the insert table with empty lists for each kernel chain.  Ensures that we
	// clean up any chains that we hooked on a previous run.
	inserts := map[string][]Rule{}
	dirtyInserts := set.New()
	for _, kernelChain := range tableToKernelChains[name] {
		inserts[kernelChain] = []Rule{}
		dirtyInserts.Add(kernelChain)
	}

	table := &Table{
		Name:                   name,
		IPVersion:              ipVersion,
		chainToInsertedRules:   inserts,
		dirtyInserts:           dirtyInserts,
		chainNameToChain:       map[string]*Chain{},
		dirtyChains:            set.New(),
		chainToDataplaneHashes: map[string][]string{},
		logCxt: log.WithFields(log.Fields{
			"ipVersion": ipVersion,
			"table":     name,
		}),
		hashCommentPrefix: hashPrefix,
		hashCommentRegexp: hashCommentRegexp,
		ourChainsRegexp:   ourChainsRegexp,
		oldInsertRegexp:   oldInsertRegexp,

		newCmd: newCmd,
	}

	if ipVersion == 4 {
		table.iptablesRestoreCmd = "iptables-restore"
		table.iptablesSaveCmd = "iptables-save"
	} else {
		table.iptablesRestoreCmd = "ip6tables-restore"
		table.iptablesSaveCmd = "ip6tables-save"
	}
	return table
}

func (t *Table) SetRuleInsertions(chainName string, rules []Rule) {
	t.logCxt.WithField("chainName", chainName).Debug("Updating rule insertions")
	t.chainToInsertedRules[chainName] = rules
	t.dirtyInserts.Add(chainName)
}

func (t *Table) UpdateChains(chains []*Chain) {
	for _, chain := range chains {
		t.UpdateChain(chain)
	}
}

func (t *Table) UpdateChain(chain *Chain) {
	t.logCxt.WithField("chainName", chain.Name).Info("Queueing update of chain.")
	t.chainNameToChain[chain.Name] = chain
	t.dirtyChains.Add(chain.Name)
}

func (t *Table) RemoveChains(chains []*Chain) {
	for _, chain := range chains {
		t.RemoveChainByName(chain.Name)
	}
}

func (t *Table) RemoveChainByName(name string) {
	t.logCxt.WithField("chainName", name).Info("Queing deletion of chain.")
	delete(t.chainNameToChain, name)
	t.dirtyChains.Add(name)
}

func (t *Table) loadDataplaneState() {
	// Load the hashes from the dataplane.
	t.logCxt.Info("Scanning for out-of-sync iptables chains")
	dataplaneHashes := t.getHashesFromDataplane()

	// Check that the rules we think we've programmed are still there and mark any inconsistent
	// chains for refresh.
	for chainName, expectedHashes := range t.chainToDataplaneHashes {
		logCxt := t.logCxt.WithField("chainName", chainName)
		if t.dirtyChains.Contains(chainName) {
			// Already an update pending for this chain.
			logCxt.Debug("Skipping known-dirty chain")
			continue
		}
		if !t.ourChainsRegexp.MatchString(chainName) {
			// Not one of our chains, check for any insertions.
			logCxt.Debug("Scanning chain for inserts")
			seenNonCalicoRule := false
			dirty := false
			expectedRules := t.chainToInsertedRules[chainName]
			expectedHashes := calculateRuleInsertHashes(chainName, expectedRules)
			numHashesSeen := 0
			if len(dataplaneHashes[chainName]) < len(expectedHashes) {
				// Chain is too short to contain all out rules.
				logCxt.Info("Chain too short to hold all Calico rules.")
				dirty = true
			} else {
				// Chain is long enough to contain all our rules, if we iterate
				// over the chain then we're guaranteed to check all the hashes.
				for i, hash := range dataplaneHashes[chainName] {
					if hash == "" {
						seenNonCalicoRule = true
						continue
					}
					numHashesSeen += 1
					if seenNonCalicoRule {
						// Calico rule after a non-calico rule, need to re-insert
						// our rules to move them to the top.
						logCxt.Info("Calico rules moved.")
						dirty = true
						break
					}
					if i >= len(expectedRules) {
						// More insertions than we're expecting, need to clean up.
						logCxt.Info("Found extra Calico rule insertions")
						dirty = true
						break
					}
					if hash != expectedHashes[i] {
						// Incorrect hash.
						logCxt.Info("Found incorrect Calico rule insertions.")
						dirty = true
						break
					}
				}
				if !dirty && numHashesSeen != len(expectedHashes) {
					logCxt.Info("Chain has incorrect number of insertions.")
					dirty = true
				}
			}
			if dirty {
				logCxt.Info("Marking chain for refresh.")
				t.dirtyInserts.Add(chainName)
			}
		} else {
			// One of our chains, should match exactly.
			if !reflect.DeepEqual(dataplaneHashes[chainName], expectedHashes) {
				logCxt.Warn("Detected out-of-sync Calico chain, marking for resync")
				t.dirtyChains.Add(chainName)
			}
		}

	}

	// Now scan for chains that shouldn't be there and mark for deletion.
	t.logCxt.Info("Scanning for unexpected iptables chains")
	for chainName := range dataplaneHashes {
		logCxt := t.logCxt.WithField("chainName", chainName)
		if t.dirtyChains.Contains(chainName) || t.dirtyInserts.Contains(chainName) {
			// Already an update pending for this chain.
			logCxt.Debug("Skipping known-dirty chain")
			continue
		}
		if !t.ourChainsRegexp.MatchString(chainName) {
			// Skip non-felix chain
			logCxt.Debug("Skipping non-calico chain")
			continue
		}
		if _, ok := t.chainToDataplaneHashes[chainName]; ok {
			// Chain expected, we'll have checked its contents above.
			logCxt.Debug("Skipping expected chain")
			continue
		}
		// Chain exists in dataplane but not in memory, mark as dirty so we'll clean it up.
		logCxt.Info("Found unexpected chain, marking for cleanup")
		t.dirtyChains.Add(chainName)
	}

	t.logCxt.Info("Done scanning, in sync with dataplane")
	t.chainToDataplaneHashes = dataplaneHashes
	t.inSyncWithDataPlane = true
}

// getHashesFromDataplane loads the current state of our table and parses out the hashes that we
// add to rules.  It returns a map with an entry for each chain in the table.  Each entry is a slice
// containing the hashes for the rules in that table.  Rules with no hashes are represented by
// an empty string.
func (t *Table) getHashesFromDataplane() map[string][]string {
	cmd := t.newCmd(t.iptablesSaveCmd, "-t", t.Name)
	output, err := cmd.Output()
	if err != nil {
		t.logCxt.WithError(err).Panic("iptables save failed")
	}
	buf := bytes.NewBuffer(output)
	return t.getHashesFromBuffer(buf)
}

// getHashesFromBuffer parses a buffer containing iptables-save output for this table, extracting
// our rule hashes.  Entries in the returned map are indexed by chain name.  For rules that we
// wrote, the hash is extracted from a comment that we added to the rule.  For rules written by
// previous versions of Felix, returns a dummy non-zero value.  For rules not written by Felix,
// returns a zero string.  Hence, the lengths of the returned values are the lengths of the chains
// whether written by Felix or not.
func (t *Table) getHashesFromBuffer(buf *bytes.Buffer) map[string][]string {
	newHashes := map[string][]string{}
	for {
		// Read the next line of the output.
		line, err := buf.ReadString('\n')
		if err != nil { // EOF
			break
		}

		// Look for lines of the form ":chain-name - [0:0]", which are forward declarations
		// for (possibly empty) chains.
		logCxt := t.logCxt.WithField("line", line)
		logCxt.Debug("Parsing line")
		captures := chainCreateRegexp.FindStringSubmatch(line)
		if captures != nil {
			// Chain forward-reference, make sure the chain exists.
			chainName := captures[1]
			logCxt.WithField("chainName", chainName).Debug("Found forward-reference")
			newHashes[chainName] = []string{}
			continue
		}

		// Look for append lines, such as "-A chain-name -m foo --foo bar"; these are the
		// actual rules.
		captures = appendRegexp.FindStringSubmatch(line)
		if captures == nil {
			// Skip any non-append lines.
			logCxt.Debug("Not an append, skipping")
			continue
		}
		chainName := captures[1]

		// Look for one of our hashes on the rule.  We record a zero hash for unknown rules
		// so that they get cleaned up.  Note: we're implicitly capturing the first match
		// of the regex.  When writing the rules, we ensure that the hash is written as the
		// first comment.
		hash := ""
		captures = t.hashCommentRegexp.FindStringSubmatch(line)
		if captures != nil {
			hash = captures[1]
			logCxt.WithField("hash", hash).Debug("Found hash in rule")
		} else if t.oldInsertRegexp.FindString(line) != "" {
			logCxt.WithFields(log.Fields{
				"rule":      line,
				"chainName": chainName,
			}).Info("Found inserted rule from previous Felix version, marking for cleanup.")
			hash = "OLD INSERT RULE"
		}
		newHashes[chainName] = append(newHashes[chainName], hash)
	}
	t.logCxt.Debugf("Read hashes from dataplane: %#v", newHashes)
	return newHashes
}

func (t *Table) InvalidateDataplaneCache() {
	t.inSyncWithDataPlane = false
}

func (t *Table) Apply() {
	// Retry until we succeed.  There are several reasons that updating iptables may fail:
	// - a concurrent write may invalidate iptables-restore's compare-and-swap
	// - another process may have clobbered some of our state, resulting in inconsistencies
	//   in what we try to program.
	retries := 10
	backoffTime := 1 * time.Millisecond
	failedAtLeastOnce := false
	for {
		if !t.inSyncWithDataPlane {
			// We have reason to believe that our picture of the dataplane is out of
			// sync.  Refresh it.  This may mark more chains as dirty.
			t.loadDataplaneState()
		}

		if err := t.applyUpdates(); err != nil {
			if retries > 0 {
				retries--
				t.logCxt.WithError(err).Warn("Failed to program iptables, will retry")
				time.Sleep(backoffTime)
				backoffTime *= 2
				t.logCxt.WithError(err).Warn("Retrying...")
				failedAtLeastOnce = true
				continue
			} else {
				t.logCxt.WithError(err).Panic("Failed to program iptables, giving up after retries")
			}
		}
		if failedAtLeastOnce {
			t.logCxt.Warn("Succeeded after retry.")
		}
		break
	}
}

func (t *Table) applyUpdates() error {
	var inputBuf bytes.Buffer
	// iptables-restore input starts with a line indicating the table name.
	tableNameLine := fmt.Sprintf("*%s\n", t.Name)
	inputBuf.WriteString(tableNameLine)

	// Make a pass over the dirty chains and generate a forward reference for any that need to
	// be created or flushed.
	t.dirtyChains.Iter(func(item interface{}) error {
		chainName := item.(string)
		chainNeedsToBeFlushed := false
		if _, ok := t.chainNameToChain[chainName]; !ok {
			// About to delete this chain, flush it first to sever dependencies.
			chainNeedsToBeFlushed = true
		} else if _, ok := t.chainToDataplaneHashes[chainName]; !ok {
			// Chain doesn't exist in dataplane, mark it for creation.
			chainNeedsToBeFlushed = true
		}
		if chainNeedsToBeFlushed {
			inputBuf.WriteString(fmt.Sprintf(":%s - -\n", chainName))
		}
		return nil
	})

	// Make a second pass over the dirty chains.  This time, we write out the rule changes.
	newHashes := map[string][]string{}
	t.dirtyChains.Iter(func(item interface{}) error {
		chainName := item.(string)
		if chain, ok := t.chainNameToChain[chainName]; ok {
			// Chain update or creation.  Scan the chain against its previous hashes
			// and replace/append/delete as appropriate.
			previousHashes := t.chainToDataplaneHashes[chainName]
			currentHashes := chain.RuleHashes()
			newHashes[chainName] = currentHashes
			for i := 0; i < len(previousHashes) || i < len(currentHashes); i++ {
				var line string
				if i < len(previousHashes) && i < len(currentHashes) {
					if previousHashes[i] == currentHashes[i] {
						continue
					}
					// Hash doesn't match, replace the rule.
					ruleNum := i + 1 // 1-indexed.
					prefixFrag := t.commentFrag(currentHashes[i])
					line = chain.Rules[i].RenderReplace(chainName, ruleNum, prefixFrag)
				} else if i < len(previousHashes) {
					// previousHashes was longer, remove the old rules from the end.
					ruleNum := len(currentHashes) + 1 // 1-indexed
					line = deleteRule(chainName, ruleNum)
				} else {
					// currentHashes was longer.  Append.
					prefixFrag := t.commentFrag(currentHashes[i])
					line = chain.Rules[i].RenderAppend(chainName, prefixFrag)
				}
				inputBuf.WriteString(line)
				inputBuf.WriteString("\n")
			}
		}
		return nil // Delay clearing the set until we've programmed iptables.
	})

	// Now calculate iptables updates for our inserted rules, which are used to hook top-level
	// chains.
	t.dirtyInserts.Iter(func(item interface{}) error {
		chainName := item.(string)
		previousHashes := t.chainToDataplaneHashes[chainName]

		// Calculate the hashes for our inserted rules.
		rules := t.chainToInsertedRules[chainName]
		currentHashes := calculateRuleInsertHashes(chainName, rules)

		needsRewrite := false
		if len(previousHashes) < len(currentHashes) ||
			!reflect.DeepEqual(currentHashes, previousHashes[:len(currentHashes)]) {
			// Hashes are wrong, rules need to be re-inserted.
			t.logCxt.WithField("chainName", chainName).Info("Inserted rules changed, updating.")
			needsRewrite = true
		} else {
			// Hashes were correct for the first few rules, check whether there are any
			// stray rules further down the chain.
			for i := len(currentHashes); i < len(previousHashes); i++ {
				if previousHashes[i] != "" {
					t.logCxt.WithField("chainName", chainName).Info(
						"Chain contains old rule insertion, updating.")
					needsRewrite = true
					break
				}
			}
		}
		if !needsRewrite {
			// Chain is in sync, skip to next one.
			return nil
		}

		// For simplicity, if we've discovered that we're out-of-sync, remove all our
		// inserts from this chain and re-insert them.  Need to remove/insert in reverse
		// order to preserve rule numbers until we're finished.  This clobbers rule counters
		// but it should be very rare (startup-only unless someone is altering our rules).
		for i := len(previousHashes) - 1; i >= 0; i-- {
			if previousHashes[i] != "" {
				ruleNum := i + 1
				line := deleteRule(chainName, ruleNum)
				inputBuf.WriteString(line)
				inputBuf.WriteString("\n")
			} else {
				// Make sure currentHashes ends up the right length.
				currentHashes = append(currentHashes, "")
			}
		}
		for i := len(rules) - 1; i >= 0; i-- {
			prefixFrag := t.commentFrag(currentHashes[i])
			line := rules[i].RenderInsert(chainName, prefixFrag)
			inputBuf.WriteString(line)
			inputBuf.WriteString("\n")
		}
		newHashes[chainName] = currentHashes

		return nil // Delay clearing the set until we've programmed iptables.
	})

	// Do deletions at the end.  This ensures that we don't try to delete any chains that
	// are still referenced (because we'll have removed the references in the modify pass
	// above).  Note: if a chain is being deleted at the same time as a chain that it refers to
	// then we'll issue a create+flush instruction in the very first pass, which will sever the
	// references.
	t.dirtyChains.Iter(func(item interface{}) error {
		chainName := item.(string)
		if _, ok := t.chainNameToChain[chainName]; !ok {
			// Chain deletion
			inputBuf.WriteString(fmt.Sprintf("--delete-chain %s\n", chainName))
			newHashes[chainName] = nil
		}
		return nil // Delay clearing the set until we've programmed iptables.
	})

	if inputBuf.Len() > len(tableNameLine) {
		// We've figured out that we need to make some changes, finish off the input then
		// execute iptables-restore.  iptables-restore input ends with a COMMIT.
		inputBuf.WriteString("COMMIT\n")

		// Annoying to have to copy the buffer here but reading from a buffer is
		// destructive so if we want to trace out the contents after a failure, we have to
		// take a copy.
		input := inputBuf.String()
		t.logCxt.WithField("iptablesInput", input).Debug("Writing to iptables")

		var outputBuf, errBuf bytes.Buffer
		cmd := t.newCmd(t.iptablesRestoreCmd, "--noflush", "--verbose")
		cmd.SetStdin(&inputBuf)
		cmd.SetStdout(&outputBuf)
		cmd.SetStderr(&errBuf)
		err := cmd.Run()
		if err != nil {
			t.logCxt.WithFields(log.Fields{
				"output":      outputBuf.String(),
				"errorOutput": errBuf.String(),
				"error":       err,
				"input":       input,
			}).Warn("Failed to execute ip(6)tables-restore command")
			t.inSyncWithDataPlane = false
			return err
		}
	}

	// TODO(smc) Do a local retry of COMMIT errors since they're expected and common if others
	// are modifying iptables.

	// Now we've successfully updated iptables, clear the dirty sets.  We do this even if we
	// found there was nothing to do above, since we may have found out that a dirty chain
	// was actually a no-op update.
	t.dirtyChains = set.New()
	t.dirtyInserts = set.New()

	// Store off the updates.
	for chainName, hashes := range newHashes {
		if hashes == nil {
			delete(t.chainToDataplaneHashes, chainName)
		} else {
			t.chainToDataplaneHashes[chainName] = hashes
		}
	}
	return nil
}

func (t *Table) commentFrag(hash string) string {
	return fmt.Sprintf(`-m comment --comment "%s%s"`, t.hashCommentPrefix, hash)
}

func deleteRule(chainName string, ruleNum int) string {
	return fmt.Sprintf("-D %s %d", chainName, ruleNum)
}

func calculateRuleInsertHashes(chainName string, rules []Rule) []string {
	chain := Chain{
		Name:  chainName,
		Rules: rules,
	}
	return (&chain).RuleHashes()
}