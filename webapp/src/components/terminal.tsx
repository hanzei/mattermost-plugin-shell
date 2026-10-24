// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {FitAddon} from '@xterm/addon-fit';
import {Terminal} from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import manifest from 'manifest';
import React, {useCallback, useEffect, useRef, useState} from 'react';

import type {ClusterNode} from './cluster_nodes';
import {pickNode, saveNode} from './cluster_nodes';

import {closeRHS} from '../rhs';

// Output flows backend -> browser as raw binary frames written straight to
// xterm. Control flows browser -> backend as JSON envelopes.
type ClientMessage =
    | {type: 'data'; data: string}
    | {type: 'resize'; cols: number; rows: number};

type ShellInfo = {name: string; path: string};

type ShellsResponse = {shells: ShellInfo[]; selected: string};

function pluginURL(path: string, params?: URLSearchParams): string {
    const query = params && params.toString() ? `?${params.toString()}` : '';
    return `/plugins/${manifest.id}/api/v1/${path}${query}`;
}

// routeParams asks the plugin to run on a specific cluster node. The node list
// lets the receiving node recognize itself, which the plugin API can't tell it.
function routeParams(node: string, nodes: ClusterNode[]): URLSearchParams {
    const params = new URLSearchParams();
    if (node) {
        params.set('node', node);
        params.set('nodes', JSON.stringify(nodes));
    }
    return params;
}

function terminalSocketURL(shell: string, params: URLSearchParams): string {
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    if (shell) {
        params.set('shell', shell);
    }
    return `${proto}//${window.location.host}${pluginURL('terminal', params)}`;
}

const selectStyle: React.CSSProperties = {
    background: '#2b2b2b',
    color: '#ddd',
    border: '1px solid #555',
    borderRadius: 4,
    padding: '1px 4px',
};

const HostTerminal: React.FC = () => {
    const containerRef = useRef<HTMLDivElement | null>(null);
    const termRef = useRef<Terminal | null>(null);
    const fitRef = useRef<FitAddon | null>(null);
    const socketRef = useRef<WebSocket | null>(null);

    // Cluster nodes; empty unless the server is a cluster of more than one.
    const [nodes, setNodes] = useState<ClusterNode[]>([]);

    // The node picked: null until the node list has loaded, '' when there is no
    // choice to make (a single server).
    const [node, setNode] = useState<string | null>(null);

    // The shells on the picked node. Empty while they load.
    const [shells, setShells] = useState<ShellInfo[]>([]);

    // What the socket connects to. A shell name only means something on the
    // node it was listed for, so the two change together: picking another node
    // keeps the old session until that node's shells are known.
    const [target, setTarget] = useState<{node: string; shell: string} | null>(null);
    const [status, setStatus] = useState<'connecting' | 'open' | 'closed'>('connecting');

    const send = useCallback((msg: ClientMessage) => {
        const socket = socketRef.current;
        if (socket && socket.readyState === WebSocket.OPEN) {
            socket.send(JSON.stringify(msg));
        }
    }, []);

    const fitAndResize = useCallback(() => {
        const fit = fitRef.current;
        const term = termRef.current;
        if (!fit || !term) {
            return;
        }
        try {
            fit.fit();
        } catch {
            return; // container not laid out yet
        }
        send({type: 'resize', cols: term.cols, rows: term.rows});
    }, [send]);

    // Create the xterm instance once; it persists across shell switches.
    useEffect(() => {
        if (!containerRef.current) {
            return undefined;
        }

        const term = new Terminal({
            cursorBlink: true,
            fontSize: 13,
            fontFamily: 'Menlo, Monaco, Consolas, "Courier New", monospace',
            theme: {background: '#1e1e1e'},
        });
        const fit = new FitAddon();
        term.loadAddon(fit);
        term.open(containerRef.current);
        fit.fit();

        termRef.current = term;
        fitRef.current = fit;

        const dataSub = term.onData((data) => send({type: 'data', data}));
        const onWindowResize = () => fitAndResize();
        window.addEventListener('resize', onWindowResize);

        return () => {
            window.removeEventListener('resize', onWindowResize);
            dataSub.dispose();
            term.dispose();
            termRef.current = null;
            fitRef.current = null;
        };
    }, [send, fitAndResize]);

    // Load the cluster's nodes. Single servers return an empty list.
    useEffect(() => {
        let cancelled = false;
        fetch('/api/v4/cluster/status', {
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        }).
            then((res) => (res.ok ? res.json() : [])).
            then((data: ClusterNode[]) => {
                if (cancelled) {
                    return;
                }

                // Nodes without an address are registered but not connected,
                // so a shell can't run on them.
                const connected = (Array.isArray(data) ? data : []).
                    filter((n) => n.ipaddress).
                    map(({id, hostname, ipaddress}) => ({id, hostname, ipaddress})).
                    sort((a, b) => a.hostname.localeCompare(b.hostname));
                if (connected.length < 2) {
                    setNode('');
                    return;
                }
                setNodes(connected);
                setNode(pickNode(connected).id);
            }).
            catch(() => {
                // Not a cluster, or no permission: the node picker stays hidden.
                if (!cancelled) {
                    setNode('');
                }
            });
        return () => {
            cancelled = true;
        };
    }, []);

    // Load the available shells and the saved selection for the chosen node.
    useEffect(() => {
        if (node === null) {
            return undefined; // wait for the node list
        }
        let cancelled = false;
        fetch(pluginURL('shells', routeParams(node, nodes)), {credentials: 'same-origin'}).
            then((res) => res.json()).
            then((data: ShellsResponse) => {
                if (cancelled) {
                    return;
                }
                setShells(data.shells || []);
                setTarget({node, shell: data.selected || ''});
            }).
            catch(() => {
                if (!cancelled) {
                    setTarget({node, shell: ''}); // fall back to the server default
                }
            });
        return () => {
            cancelled = true;
        };
    }, [node, nodes]);

    // Connect the socket once a target is known; reconnect when it changes.
    useEffect(() => {
        if (target === null) {
            return undefined; // wait for the shells fetch to resolve
        }
        const term = termRef.current;
        if (!term) {
            return undefined;
        }

        term.reset();
        setStatus('connecting');

        let opened = false;
        let failed = false;
        const socket = new WebSocket(terminalSocketURL(target.shell, routeParams(target.node, nodes)));
        socket.binaryType = 'arraybuffer';
        socketRef.current = socket;

        socket.onopen = () => {
            opened = true;
            setStatus('open');
            send({type: 'resize', cols: term.cols, rows: term.rows});
            term.focus();
        };
        socket.onmessage = (event: MessageEvent) => {
            if (event.data instanceof ArrayBuffer) {
                term.write(new Uint8Array(event.data));
            } else if (typeof event.data === 'string') {
                // Text frames are errors from the plugin, e.g. a node that
                // could not start the shell.
                term.write(`\r\n\x1b[31m[${event.data}]\x1b[0m\r\n`);
                failed = true;
            }
        };
        socket.onclose = () => {
            setStatus('closed');

            // A session that actually started and then ended (e.g. the user
            // typed `exit`) closes the panel. A socket that never opened is a
            // connection problem worth surfacing instead.
            if (opened && !failed) {
                closeRHS();
            } else if (!opened) {
                term.write('\r\n\x1b[31m[could not connect to terminal]\x1b[0m\r\n');
            }
        };

        return () => {
            socket.onclose = null; // don't close the panel when switching shells
            socket.close();
            socketRef.current = null;
        };
    }, [target, nodes, send]);

    const nodeName = nodes.find((n) => n.id === target?.node)?.hostname;

    const wrapperStyle: React.CSSProperties = {
        display: 'flex',
        flexDirection: 'column',
        height: '100%',
        background: '#1e1e1e',
    };

    return (
        <div style={wrapperStyle}>
            <div
                style={{
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'space-between',
                    gap: 8,
                    padding: '6px 10px',
                    color: '#ddd',
                    fontSize: 12,
                    borderBottom: '1px solid #333',
                }}
            >
                <span>{'Shell · '}{status}{nodeName ? ` · ${nodeName}` : ''}</span>
                {nodes.length > 0 && (
                    <label style={{display: 'flex', alignItems: 'center', gap: 4, marginLeft: 'auto'}}>
                        <span>{'Node:'}</span>
                        <select
                            value={node ?? ''}
                            onChange={(e) => {
                                const picked = nodes.find((n) => n.id === e.target.value);
                                if (picked) {
                                    saveNode(picked);
                                    setShells([]);
                                    setNode(picked.id);
                                }
                            }}
                            style={selectStyle}
                        >
                            {nodes.map((n) => (
                                <option
                                    key={n.id}
                                    value={n.id}
                                >
                                    {n.hostname}
                                </option>
                            ))}
                        </select>
                    </label>
                )}
                {shells.length > 0 && (
                    <label style={{display: 'flex', alignItems: 'center', gap: 4}}>
                        <span>{'Shell:'}</span>
                        <select
                            value={target?.shell ?? ''}
                            onChange={(e) => node !== null && setTarget({node, shell: e.target.value})}
                            style={selectStyle}
                        >
                            {shells.map((s) => (
                                <option
                                    key={s.name}
                                    value={s.name}
                                >
                                    {s.name}
                                </option>
                            ))}
                        </select>
                    </label>
                )}
            </div>
            <div
                ref={containerRef}
                style={{flex: 1, minHeight: 0, padding: 4}}
            />
        </div>
    );
};

export default HostTerminal;
