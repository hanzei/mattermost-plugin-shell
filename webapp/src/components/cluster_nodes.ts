// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import manifest from 'manifest';

// One entry of /api/v4/cluster/status.
export type ClusterNode = {id: string; hostname: string; ipaddress: string};

// The last node picked, kept per browser. Node IDs change when a node
// restarts, so the host name is kept too and used as a fallback.
const savedNodeKey = `${manifest.id}:node`;

function loadSavedNode(): {id?: string; hostname?: string} {
    try {
        return JSON.parse(localStorage.getItem(savedNodeKey) || '{}');
    } catch {
        return {};
    }
}

export function saveNode(node: ClusterNode) {
    try {
        localStorage.setItem(savedNodeKey, JSON.stringify({id: node.id, hostname: node.hostname}));
    } catch {
        // Storage unavailable: the choice just won't survive a refresh.
    }
}

// pickNode returns the saved node if it is still in the cluster, else the first.
export function pickNode(nodes: ClusterNode[]): ClusterNode {
    const saved = loadSavedNode();
    return nodes.find((n) => n.id === saved.id) ||
        nodes.find((n) => n.hostname === saved.hostname) ||
        nodes[0];
}
