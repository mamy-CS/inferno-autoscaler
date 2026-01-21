import random

def generate_exp1_log(filename):
    with open(filename, 'w') as f:
        # Phase 1: Steady state (2 replicas)
        for i in range(10):
            f.write(f"Desired Optimized Allocation for VA: llm-d-sim-deployment - Replicas: 2, Accelerator: A100\n")
        
        # Phase 2: Reactivity Scale Up (Load hits)
        for i in range(5):
             f.write(f"Desired Optimized Allocation for VA: llm-d-sim-deployment - Replicas: 4, Accelerator: A100\n")
        
        # Phase 3: Saturation Scale Up
        for i in range(10):
             f.write(f"Desired Optimized Allocation for VA: llm-d-sim-deployment - Replicas: 8, Accelerator: A100\n")
             
        # Phase 4: Stabilization
        for i in range(10):
             f.write(f"Desired Optimized Allocation for VA: llm-d-sim-deployment - Replicas: 8, Accelerator: A100\n")

def generate_exp2_log(filename):
    with open(filename, 'w') as f:
        # Phase 1: Zero
        for i in range(5):
            f.write(f"Desired Optimized Allocation for VA: llm-d-sim-a100-deployment - Replicas: 0, Accelerator: A100\n")
            f.write(f"Desired Optimized Allocation for VA: llm-d-sim-h100-deployment - Replicas: 0, Accelerator: H100\n")
            
        # Phase 2: A100 Scales First (Cheaper)
        for i in range(10):
            # Ramp up
            replicas = min(i + 1, 4)
            f.write(f"Desired Optimized Allocation for VA: llm-d-sim-a100-deployment - Replicas: {replicas}, Accelerator: A100\n")
            f.write(f"Desired Optimized Allocation for VA: llm-d-sim-h100-deployment - Replicas: 0, Accelerator: H100\n")
            
        # Phase 3: A100 Maxed, Spillover to H100
        for i in range(10):
            f.write(f"Desired Optimized Allocation for VA: llm-d-sim-a100-deployment - Replicas: 4, Accelerator: A100\n")
            h100_replicas = min(i + 1, 4)
            f.write(f"Desired Optimized Allocation for VA: llm-d-sim-h100-deployment - Replicas: {h100_replicas}, Accelerator: H100\n")

if __name__ == "__main__":
    generate_exp1_log("synthetic_exp1.log")
    generate_exp2_log("synthetic_exp2.log")
