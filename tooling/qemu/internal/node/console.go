package node

// Has reports whether the console, so far, has one line with every fragment in order.
func (n *Node) Has(fragments ...string) bool { return hasLine(n.Console(), fragments) }

// HasAfter reports whether the console after byte offset from has one line with every fragment in
// order. It limits the search to output written after a known point, so a check for "X was not
// removed" does not match a removal from an earlier pass.
func (n *Node) HasAfter(from int, fragments ...string) bool {
	cons := n.Console()
	if from > len(cons) {
		from = len(cons)
	}
	return hasLine(cons[from:], fragments)
}
