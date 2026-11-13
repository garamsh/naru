package node

// Has reports whether the console, so far, has one line with every fragment in order.
func (n *Node) Has(fragments ...string) bool { return hasLine(n.Console(), fragments) }
