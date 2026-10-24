/**
 * @jest-environment jsdom
 */

// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {pickNode, saveNode} from './cluster_nodes';

const nodes = [
    {id: 'id-a', hostname: '10.0.0.1', ipaddress: '10.0.0.1:8074'},
    {id: 'id-b', hostname: '10.0.0.2', ipaddress: '10.0.0.2:8074'},
];

beforeEach(() => localStorage.clear());

test('picks the first node when nothing was saved', () => {
    expect(pickNode(nodes)).toBe(nodes[0]);
});

test('picks the last selected node', () => {
    saveNode(nodes[1]);
    expect(pickNode(nodes)).toBe(nodes[1]);
});

test('finds the last selected node by host name after it restarted with a new ID', () => {
    saveNode({...nodes[1], id: 'old-id'});
    expect(pickNode(nodes)).toBe(nodes[1]);
});

test('falls back to the first node when the saved one is gone', () => {
    saveNode({id: 'gone', hostname: '10.0.0.9', ipaddress: '10.0.0.9:8074'});
    expect(pickNode(nodes)).toBe(nodes[0]);
});
