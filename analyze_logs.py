import re
import matplotlib.pyplot as plt
import sys
import datetime

def parse_logs(filename):
    timestamps = []
    replicas = {}
    
    # Regex for the allocation line
    alloc_pattern = re.compile(r"Desired Optimized Allocation for VA: (\S+) - Replicas: (\d+), Accelerator: (\S+)")
    
    with open(filename, 'r') as f:
        for line in f:
            match = alloc_pattern.search(line)
            if match:
                va_name = match.group(1)
                count = int(match.group(2))
                acc = match.group(3)
                
                # Check if variant exists in dict
                if va_name not in replicas:
                    replicas[va_name] = {'x': [], 'y': [], 'acc': acc}
                
                # Use simple counter for X-axis
                replicas[va_name]['y'].append(count)
                replicas[va_name]['x'].append(len(replicas[va_name]['x'])) # Step count

    return replicas

def plot_reactivity(data, output_file):
    plt.figure(figsize=(10, 6))
    for va, values in data.items():
        label_txt = f"{va} ({values.get('acc', 'Unknown')})"
        plt.plot(values['x'], values['y'], label=label_txt, marker='o')
    
    plt.title("Experiment 1: Saturation Reactivity")
    plt.xlabel("Sampling Steps (approx 15s interval)")
    plt.ylabel("Replica Count")
    plt.legend()
    plt.grid(True)
    plt.savefig(output_file)
    print(f"Generated {output_file}")

def plot_cost(data, output_file):
    plt.figure(figsize=(10, 6))
    
    # Plot each variant
    for va, values in data.items():
        acc = values.get('acc', 'Unknown')
        label_txt = f"{va} ({acc})"
        plt.plot(values['x'], values['y'], label=label_txt, marker='x', linewidth=2)
    
    plt.title("Experiment 2: Cost-Aware Scaling")
    plt.xlabel("Sampling Steps (approx 15s interval)")
    plt.ylabel("Replica Count")
    plt.legend()
    plt.grid(True)
    plt.savefig(output_file)
    print(f"Generated {output_file}")

if __name__ == "__main__":
    if len(sys.argv) < 3:
        print("Usage: python analyze_logs.py <log_file> <output_image> [mode]")
        sys.exit(1)
        
    log_file = sys.argv[1]
    output_file = sys.argv[2]
    mode = "reactivity"
    if len(sys.argv) > 3:
        mode = sys.argv[3]
    
    data = parse_logs(log_file)
    
    if mode == "cost":
        plot_cost(data, output_file)
    else:
        plot_reactivity(data, output_file)
