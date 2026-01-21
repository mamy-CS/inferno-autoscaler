
import json
import matplotlib.pyplot as plt
import numpy as np

# Load data
with open('efficiency_results.json', 'r') as f:
    data = json.load(f)

time_steps = data['time_steps']
baseline1_queue = data['baseline1_queue']
baseline2_kv = [x * 100 for x in data['baseline2_kv']] # convert to %
wva_queue = data['wva_queue']
wva_kv = [x * 100 for x in data['wva_kv']] # convert to %
wva_replicas = data['wva_replicas']

# Create figure
fig, (ax1, ax2, ax3) = plt.subplots(3, 1, figsize=(10, 12), sharex=True)

# Plot 1: Queue Depth
ax1.plot(time_steps, baseline1_queue, label='Baseline (1 Replica)', color='red', linestyle='--')
ax1.plot(time_steps, wva_queue, label='WVA', color='green', linewidth=2)
ax1.set_ylabel('Queue Depth')
ax1.set_title('Congestion Mitigation (Queue Depth)')
ax1.legend()
ax1.grid(True, alpha=0.3)
ax1.axhline(y=120, color='red', linestyle=':', alpha=0.5)
ax1.axhline(y=20, color='green', linestyle=':', alpha=0.5)

# Plot 2: KV Cache Utilization
ax2.plot(time_steps, baseline2_kv, label='Baseline (12 Replicas)', color='orange', linestyle='--')
ax2.plot(time_steps, wva_kv, label='WVA', color='blue', linewidth=2)
ax2.set_ylabel('KV Cache Utilization (%)')
ax2.set_title('Resource Efficiency (KV Cache)')
ax2.legend()
ax2.grid(True, alpha=0.3)
ax2.set_ylim(0, 100)

# Plot 3: Replicas (Proxy for Energy)
baseline_replicas = [12] * len(time_steps)
ax3.plot(time_steps, baseline_replicas, label='Baseline (Over-provisioned)', color='grey', linestyle='--')
ax3.plot(time_steps, wva_replicas, label='WVA Replicas', color='purple', linewidth=2)
ax3.fill_between(time_steps, wva_replicas, baseline_replicas, alpha=0.2, color='grey', label='Energy Savings')
ax3.set_ylabel('Replica Count')
ax3.set_title('Scaling & Energy Consumption')
ax3.set_xlabel('Time (s)')
ax3.legend()
ax3.grid(True, alpha=0.3)
ax3.set_ylim(0, 15)

plt.tight_layout()
plt.savefig('exp3_efficiency.pdf')
print("Plot saved to exp3_efficiency.pdf")
