// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import manifest from 'manifest';
import React from 'react';
import type {Store} from 'redux';

import type {GlobalState} from '@mattermost/types/store';

import type {PluginRegistry} from 'types/mattermost-webapp';

import HostTerminal from './components/terminal';

// Terminal/console glyph. Uses currentColor so it inherits the channel-header
// icon color in both the normal and active/hover states (a plain text span
// renders invisible against the white hover background).
const TerminalIcon: React.FC = () => (
    <svg
        width={24}
        height={24}
        viewBox='0 0 24 24'
        fill='none'
        stroke='currentColor'
        strokeWidth={2}
        strokeLinecap='round'
        strokeLinejoin='round'
        aria-hidden={true}
    >
        <polyline points='4 17 10 11 4 5'/>
        <line
            x1='12'
            y1='19'
            x2='20'
            y2='19'
        />
    </svg>
);

export default class Plugin {
    public async initialize(registry: PluginRegistry, store: Store<GlobalState>) {
        // @see https://developers.mattermost.com/extend/plugins/webapp/reference/
        const {toggleRHSPlugin} = registry.registerRightHandSidebarComponent(
            HostTerminal,
            'Host Terminal',
        );

        registry.registerChannelHeaderButtonAction(
            <TerminalIcon/>,
            () => store.dispatch(toggleRHSPlugin),
            'Host Terminal',
            'Open an interactive host terminal (system admins only)',
        );
    }
}

declare global {
    interface Window {
        registerPlugin(pluginId: string, plugin: Plugin): void;
    }
}

window.registerPlugin(manifest.id, new Plugin());
