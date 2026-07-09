"""Painéis educacionais — re-exporta todos os painéis."""

from mirbftview.qt.panels.info_panel import InfoPanel
from mirbftview.qt.panels.buckets_panel import BucketsPanel
from mirbftview.qt.panels.execution_panel import ExecutionPanel
from mirbftview.qt.panels.commit_chain_panel import CommitChainPanel
from mirbftview.qt.panels.event_log_panel import EventLogPanel
from mirbftview.qt.panels.global_order_panel import GlobalOrderPanel

__all__ = [
    "InfoPanel",
    "BucketsPanel",
    "ExecutionPanel",
    "CommitChainPanel",
    "EventLogPanel",
    "GlobalOrderPanel",
]
