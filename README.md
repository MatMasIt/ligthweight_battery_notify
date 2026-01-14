# Liwghtweight Battery Monitor

A clean and simple battery monitor that shows an alert (and optionally plays a sound) when the battery is running out.

This can be useful in window managers or lightweight desktop environments without this functionality.

An alert and critical level can be configured (see [battery-monitor.yaml](battery-monitor.yaml)).

Depends only on dbus for notifications (uses filesystem polling to query battery status).