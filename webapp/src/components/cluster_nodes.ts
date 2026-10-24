// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import manifest from 'manifest';

import type {ClusterInfo} from '@mattermost/types/admin';

// The parts of a /api/v4/cluster/status entry the plugin uses.
export type ClusterNode = Pick<ClusterInfo, 'id' | 'hostname' | 'ipaddress'>;

// The last node picked, kept per browser. Node IDs change when a node
// restarts, so its gossip address and host name are kept as fallbacks.
const savedNodeKey = `${manifest.id}:node`;

function loadSavedNode(): Partial<ClusterNode> {
    try {
        return JSON.parse(localStorage.getItem(savedNodeKey) || '{}');
    } catch {
        return {};
    }
}

export function saveNode(node: ClusterNode) {
    try {
        localStorage.setItem(savedNodeKey, JSON.stringify(node));
    } catch {
        // Storage unavailable: the choice just won't survive a refresh.
    }
}

// pickNode returns the saved node if it is still in the cluster, else the first.
// After a restart it is found by gossip address, which tells apart nodes that
// share a host, and only then by host name.
export function pickNode(nodes: ClusterNode[]): ClusterNode {
    const saved = loadSavedNode();
    return nodes.find((n) => n.id === saved.id) ||
        (saved.ipaddress && nodes.find((n) => n.ipaddress === saved.ipaddress)) ||
        nodes.find((n) => n.hostname === saved.hostname) ||
        nodes[0];
}
